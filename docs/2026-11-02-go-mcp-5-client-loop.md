# MCP 函数调用循环：让模型自己决定调工具

上篇我们把面试教练的 SQLite 历史库包装成了 MCP 服务，`list_sessions`、`get_report` 三个工具端端正正摆在那儿。但工具摆好 ≠ 模型会用——模型看不见你的数据库，它只看得见对话历史。**谁来决定「现在该调哪个工具、参数怎么填」？答案是模型自己；而让模型做到这件事的机制，就是函数调用循环（function calling loop）。**

今天是「面试教练变 MCP 服务」的下篇：手写一个 MCP client，把上篇的 server 变成模型随取随用的外脑。

## 一、client 骨架：spawn 子进程，先握手再说话

MCP client 的第一步不是发请求，而是**把 server 作为子进程启动**——MCP 的 stdio 传输层，本质上就是「client 握着 server 的 stdin/stdout」：

```go
cmd := exec.CommandContext(ctx, bin, args...)
cmd.Env = os.Environ()   // 环境变量原样透传：COACH_DB_PATH 这样流到 server
cmd.Stderr = os.Stderr   // server 的报错打到前台，排障不用猜
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
cmd.Start()

// 握手：交换协议版本与双方身份，版本对不上当场失败，别带病运行
raw, err := c.call(ctx, "initialize", map[string]any{
    "protocolVersion": protocolVersion,
    "capabilities":    map[string]any{},
    "clientInfo":      map[string]any{"name": "coachclient", "version": "0.1.0"},
})
```

两个细节值得记住：`exec.CommandContext` 让 ctx 取消时子进程自动被杀，不会留孤儿进程；stderr 直接接到前台，server 端一行 `打开历史库失败` 立刻可见，而不是消失在管道里。握手成功后再补发一条 `notifications/initialized` 通知——协议要求这么做，但它按定义不该有响应，发了就走，别傻等一个永远不会来的回包。

## 二、对账与通知：两个容易漏的角落

请求发出后要读响应，但 stdin 里回来的行不全是响应——server 可能随时推通知（比如 `notifications/cancelled`）。**没有 id 的行一律跳过，有 id 的行必须和当前请求对上**，否则协议就乱了：

```go
for c.out.Scan() {
    var msg rpcWire
    if err := json.Unmarshal(c.out.Bytes(), &msg); err != nil {
        continue              // 容错：跳过非 JSON 行（server 若有调试输出）
    }
    if len(msg.ID) == 0 {
        continue              // 通知（notification）：没有 id，忽略
    }
    if string(msg.ID) != id { // 串行 client 下不该发生，发生了就是 bug
        return nil, fmt.Errorf("响应 id 不匹配: got %s, want %s", msg.ID, id)
    }
    // ...error 检查与 result 返回
}
```

我们的 client 用一把 mutex 把请求串行化，第一个响应必然对应当前请求，对账逻辑因此极简——这是「 stdio + 逐行 JSON」传输白送的便利，换成并发管道就没有这么便宜了。

## 三、函数调用循环：二十行核心

循环本身朴素得可疑：**模型要调工具就执行，把结果塞进历史再问一次，直到模型给出纯文本**：

```go
for step := 1; step <= maxSteps; step++ {
    resp, err := llm.Chat(ctx, messages, tools)
    // ...
    if len(resp.ToolCalls) == 0 {
        return resp.Text, nil   // 纯文本 = 最终答复，收工
    }
    messages = append(messages, *resp)
    for _, tc := range resp.ToolCalls {
        result, err := exec(ctx, tc.Name, tc.Arguments)
        if err != nil {
            result = "工具调用失败：" + err.Error()
        }
        messages = append(messages, Message{
            Role: "tool", ToolCallID: tc.ID, Text: result,
        })
    }
}
```

但有两处是新手必踩的坑，注释里写不下，单独说：

- **工具失败不中止循环，把错误喂回模型**。上一篇我们精心设计的引路文案（「可先调用 list_sessions 查看可用 ID」）就是给这一刻用的——模型读到错误会自己换参数重试，而不是整个流程崩掉。这正是 Agent 区别于普通 API 调用的本质。
- **必须有 maxSteps 上限**。模型完全可能陷入「反复调同一个工具」的死循环，没有护栏就是把 token 钱包交给模型的随机数发生器。

还有一个隐藏分支：模型一轮可以请求**多个**工具调用（比如同时要报告和逐题指标），我们的循环按顺序逐个执行、逐个回执，结果按 ToolCallID 排队进历史，模型自己会对号取用。想升级成并发执行也不难——Executor 本来就是函数，包一层 errgroup 即可，但 MCP server 那边是否并发安全，升级前要先确认。

## 四、收藏：循环里四种消息角色对照表

循环与具体模型供应商之间只隔一个 `LLM` 接口，接口里统一用四种消息。对照 OpenAI Chat Completions 的角色，一张表记牢：

| 循环中的 Role | OpenAI 消息类型 | 何时出现 | 关键字段 |
|---|---|---|---|
| `system` | system | 开场一次，定规矩 | Text |
| `user` | user | 用户提问 | Text |
| `assistant` | assistant | 每轮模型输出 | Text 和/或 ToolCalls |
| `tool` | tool | 每次工具执行后 | Text + ToolCallID（回执必须带原调用 ID） |

最后一行的 `ToolCallID` 是最容易漏的：模型一轮可能请求多个工具调用，**结果必须按 ID 对号入座**，否则模型分不清哪份结果对应哪次调用。我们的 `Message` 结构用同一个类型承载四种角色，换供应商时只需重写一个适配文件（仓库里 `openai_llm.go` 把双向翻译集中在一处，openai-go v3 的工具类型直接映射，MCP 的 JSON Schema 原样塞进 function 的 parameters）。

## 五、实测：没有 API key 也能端到端

测试 Agent 链路最烦的就是依赖真实模型——不稳定、要花钱、没法断言。我们的解法是内置一台**行为确定的假模型** `scriptedLLM`：第一轮要 `list_sessions`，第二轮从工具结果里正则抓场次 ID 去要 `get_report`，第三轮总结收工。它既是 main 在无 key 时的演示模式，也是端到端测试的驱动器：

```
[client] server 提供 3 个工具: list_sessions get_report get_session
[client] 未检测到 OPENAI_API_KEY，使用内置演示模型
已为你取回最近一场面试的报告：
# 面试报告 · Go 后端工程师
综合 72 分：并发维度偏弱，建议复盘 GMP 调度与 channel 用法。
```

假模型走通了，换真模型只是 `OPENAI_API_KEY` + `COACH_MODEL` 两个环境变量的事——接口早已就位。这也体现了「依赖接口」的回报：`LLM` 接口不到十行，测试替身二十行，真模型适配一个文件，三层各自独立演进。测试 5 个用例全绿：正常路径、错误喂回、maxSteps 护栏、真进程端到端、server 早退不挂死。其中「server 早退」那个用例专门防一种隐蔽 bug——子进程如果启动即崩溃，client 的读循环会收到 EOF，此时必须返回错误而不是空转挂死，否则上层只能对着一个死进程干等超时。

至此，「面试官 Agent 记得每一位候选人」闭环完成：server 管数据，client 管循环，模型管决策。下篇换个视角，聊聊 MCP 协议里三个容易被忽视但很有意思的能力：**Prompts（服务器预置提示词模板）、Sampling（反向让 server 请求模型补全）、Roots（文件系统访问边界）**。

觉得有用的话，欢迎点赞、在看、转发，关注「Go2AI之路」，下篇见。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
