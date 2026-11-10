# Sampling：让 server 反向请求你的大模型

前五篇的 MCP 都是「client 问、server 答」：工具列表、资源读取、历史查询，方向始终只有一个。但协议里还藏着三个进阶能力，其中一个是**方向反转**——server 在处理请求的过程中，反过来向 client 要一次模型补全。这就是本篇的主角：Prompts、Sampling、Roots 三件套。

先给结论性的提醒：三件套里 Prompts 和 Roots 至今好用，而 **Sampling 官方已于协议版本 2026-07-28 起将其标记废弃（SEP-2577）**，新实现不应采用。我们仍然实现并讲透它——为了看懂旧 server 的行为、练手双向 JSON-RPC、以及理解「为什么被废弃」。

## 一、Prompts：把提示词当版本化资产分发

团队里最值钱的 prompt 往往散落在各个服务的代码里。Prompts 能力让 server 把提示词模板暴露给 client：`prompts/list` 列出模板清单（名称、描述、参数表），`prompts/get` 按名称取回组装好的消息列表：

```go
case "prompts/get":
    // 参数校验同 tools：缺必填参数按协议返回 -32602
    report, ok := in.Arguments["report_md"]
    if !ok || report == "" {
        resp.Error = &rpcError{Code: -32602, Message: "缺少必填参数 report_md"}
        return nil
    }
    text := "你是一位面试复盘教练。请基于下面的评分报告输出三段复盘……\n\n报告全文：\n" + report
    return mustRaw(map[string]any{
        "messages": []any{{"role": "user",
            "content": map[string]any{"type": "text", "text": text}}},
    })
```

价值在于：prompt 从「某个工程师电脑里的字符串」变成「server 端版本化的资产」，所有 client（Claude Desktop、Cursor、你的自研 Agent）拿到的都是同一份、可审计、可灰度更新的模板。

## 二、Sampling：server 向 client 要一次模型补全

这是全系列第一次出现「server 主动发请求」。场景：我们的 `polish_report` 工具想把面试报告润色成口语化总结——润色需要模型，但 server 不该自带 API key（那是 client 的资源和密钥）。Sampling 的思路：**server 发一个 `sampling/createMessage` 请求给 client，client 用它自己的模型配额完成补全，把结果返回**。

```go
res, err := s.ask("sampling/createMessage", map[string]any{
    "messages":  []any{{"role": "user", "content": map[string]any{"type": "text", "text": prompt}}},
    "maxTokens": 300, // 规范里 client 必须尊重的参数
})
if err != nil {
    // client 拒绝或超时是常态：回 isError 让模型层看到原因，而不是崩掉整个调用
    return mustRaw(map[string]any{
        "isError": true,
        "content": []any{map[string]any{"type": "text", "text": "采样失败：" + err.Error()}},
    })
}
var sample struct { // 结果形状：role + content（text 块）
    Content struct{ Type, Text string }
}
```

听起来优雅，为什么被废弃？三个硬伤：server 借 client 的密钥和额度绕开了一切配额与审计，安全边界模糊；client 被迫实现一套模型路由（`modelPreferences` 的 hints/priority 映射到自家可用模型，复杂且各家不一）；人机审批链路（规范要求用户可预览可拒绝）几乎没有 client 真正做好。官方结论：server 需要模型能力时，直接集成 LLM API 更诚实。

但实现它的过程把双向 JSON-RPC 的功课全补上了，这些功夫在 Elicitation 等新能力里继续有效。

## 三、Roots：文件系统访问边界

Roots 解决一个很实际的问题：client 打开的工作区是哪些目录？server 声明 `roots` 能力后，可随时向 client 发起 `roots/list` 查询；client 工作区变化时主动推 `notifications/roots/list_changed`。

```go
func (s *server) queryRoots() {
    res, err := s.ask("roots/list", map[string]any{})
    // 查不到不致命——roots 是优化信息，不是运行前提
    var roots struct{ Roots []struct{ URI, Name string } }
    json.Unmarshal(res, &roots)
    for _, r := range roots.Roots { /* 记录允许的工作区边界 */ }
}
```

我们的 server 在收到 `notifications/initialized` 后顺手查一次，实测拿到 `file:///workspace`——如果 server 提供文件类工具，这个边界就是「哪些路径可读」的白名单依据。完整的客户端还应该监听 `notifications/roots/list_changed`：用户在 IDE 里切换了工作区，server 手里的边界名单要跟着刷新，否则白名单就过期成了安全隐患。

## 四、收藏：三件套速查表

| 能力 | 方向 | 关键方法 | 现实处境 |
|---|---|---|---|
| Prompts | server → client | `prompts/list` / `prompts/get` | 现役，适合分发团队提示词模板 |
| Sampling | server → client | `sampling/createMessage` | **已废弃**（2026-07-28，SEP-2577），新实现勿采用 |
| Roots | 双向 | `roots/list` + `notifications/roots/list_changed` | 现役，文件类 server 的访问边界 |

判断口诀：**要给模型「看的资料」用 Resources，要给模型「做的事」用 Tools，要给用户「选的模板」用 Prompts，文件边界问 Roots**——Sampling 留给读懂旧代码。

## 五、踩坑实录：双向化逼出来的三个 bug

把协议从一问一答改成双向对等，单线程假设全线崩塌，我们踩了三个值得记录的坑：

1. **读循环自锁**。`tools/call` 的处理函数里等采样回包，而回包要靠读循环来路由——处理函数不返回，读循环不运转，死等。解法：client→server 请求一律进 goroutine 分发，读循环只负责分拣（请求/响应/通知三类）。
2. **`json.RawMessage` 逐字节透传**。给采样请求填字符串 id 时直接 `json.RawMessage("srv-1")`，marshal 产出一个**空行**——`"srv-1"` 不是合法 JSON，错误被忽略后写了个寂寞。client 永远收不到请求。上线前必须编码：`json.Marshal(id)` 后再放进 RawMessage。
3. **pending map 的 key 形态**。等待表用原始 id 做 key，投递时拿的是带引号的 JSON 形态，两边对不上。统一编码后再比较。

配套测试 6 个用例全绿，端到端用了一个能应答反向请求的异步假 client：握手后 server 主动查 roots，`polish_report` 触发采样往返，断言采样请求带 `maxTokens`、工具结果带回润色文本、server 侧确实发出了两类反向请求。这个异步假 client 本身就是个好教具——上一篇的同步 client 在这里会直接死锁，谁需要处理 server 推送，谁就必须把「读循环」和「请求等待」拆到不同 goroutine。顺带一提：草稿版规范已把采样响应改成 `InputRequiredResult` 多轮往返模式，响应还带 `resultType`/分页字段——规范在演进，但生产环境说话的仍是 2025-06-18 稳定版，本系列代码钉死它不动摇。

至此本机 stdio 的形态已经玩透。下篇上强度：**远程 MCP——HTTP + SSE 传输、与 stdio 的差异、以及鉴权怎么做**。

觉得有用的话，欢迎点赞、在看、转发，关注「Go2AI之路」，下篇见。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
