# MCP 进阶：Tools 与 Resources 实战

上一篇我们用 200 行 Go 写了一个能握手、能回显的最小 MCP Server。但真实场景里，Agent 的工具不能只会「复读」——它要干真活；而且模型光会调工具还不够，面试前要读评分标准、查题库，这些背景知识从哪来？这就是今天要讲的 MCP 两大核心原语：**Tools（模型的「手」）和 Resources（模型的「资料库」）**。

## 一、工具注册表：告别 if-else 硬编码

minimal 版的 `tools/list` 是把工具清单手写死在 switch 分支里的，加一个新工具要改三处。进阶版用一个注册表把「声明」和「实现」收拢到一处：

```go
// 工具定义：描述 + JSON Schema + 处理函数，三件事绑在一起
type toolDef struct {
    description string
    inputSchema map[string]any   // 原样返回给宿主，最终进模型的 system prompt
    handler     func(args json.RawMessage) (string, error)
}

var registry = map[string]toolDef{} // name -> 定义

// tools/list 时遍历注册表自动生成清单，新增工具不用改这里
tools := make([]any, 0, len(registry))
for name, def := range registry {
    tools = append(tools, map[string]any{
        "name":        name,
        "description": def.description,
        "inputSchema": def.inputSchema,
    })
}
```

`inputSchema` 就是标准 JSON Schema，宿主会把它原样展示给大模型，模型据此生成合法参数。注意 `description` 是写给**模型**看的提示词，写清楚用途和约束，模型的调用成功率直接受益。比如 `calc` 的描述里写明了「除数为 0 会返回业务错误」，有经验的模型读到这句话，就会先检查参数再调用，而不是盲目撞墙再重试。

## 二、参数校验：模型给的 arguments 不可信

这是新手最容易踩的坑：`inputSchema` 只是「提示」，不是「保证」。模型完全可能少传字段、传错类型，甚至幻觉出不存在的枚举值。服务端必须逐字段校验：

```go
func handleCalc(args json.RawMessage) (string, error) {
    var in struct {
        A  *float64 `json:"a"` // 用指针区分「没传」和「传了 0」
        B  *float64 `json:"b"`
        Op string   `json:"op"`
    }
    if err := json.Unmarshal(args, &in); err != nil {
        return "", fmt.Errorf("arguments 不是合法 JSON: %w", err)
    }
    if in.A == nil || in.B == nil {
        return "", fmt.Errorf("a 和 b 都是必填的数字")
    }
    // ... 分派四则运算
    case "/":
        if *in.B == 0 {
            return "", fmt.Errorf("除数不能为 0") // 业务错误，见下一节
        }
    // ...
}
```

一个实用技巧：数值字段用 `*float64` 而非 `float64`。模型不传 `b` 时，指针是 nil；如果用普通 float，不传和传 0 无法区分，「除零检查」会漏掉一半情况。

还有一类错误是上例第一处返回的：模型传了 `"a": "abc"` 这种类型不对的 JSON。`json.Unmarshal` 会直接报错，这时你甚至拿不到部分解析的结果，只能把原始 arguments 记进日志再返回业务错误。建议开发期把模型的原始 arguments 落盘——线上排查「模型为什么不按 schema 来」时，这是最有价值的一手证据。

## 三、错误要分两层：协议错误 vs 业务错误

MCP 里表达「失败」有两条通道，用错通道是第二常见的坑：

| | JSON-RPC error | `isError: true` |
|---|---|---|
| 含义 | 协议层失败：方法不存在、参数不是合法 JSON、资源 URI 未注册 | 业务层失败：请求完全合法，但语义上无法执行 |
| 典型场景 | 调用不存在的工具名 | 除数为 0、题库没有匹配题目 |
| 谁来判断 | 宿主（客户端 SDK） | **大模型自己**读错误文本后决定怎么办 |
| 响应合法性 | 响应本身是个错误对象 | 响应完全合法，只是多一个 `isError` 字段 |

关键区别在最后一行：`isError=true` 的响应是**合法响应**，宿主会把错误文本作为工具结果展示给模型。我们的实测里，调用 `calc` 传 `{"a":1,"b":0,"op":"/"}`，拿到的就是：

```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "result": {
    "content": [{"type": "text", "text": "除数不能为 0"}],
    "isError": true
  }
}
```

模型看到这段文本后，可以自己改传 `b=2` 重试——**把恢复策略留给模型，正是 Agent 区别于普通 API 调用的地方**。

## 四、Resources：Agent 的资料库

Tools 是「执行动作」，Resources 是「暴露只读资料」：题库、评分标准、内部文档、数据表视图。宿主通过 `resources/list` 拿到 URI 清单，需要时用 `resources/read` 按 URI 取内容：

```go
// 每条资源用自定义 URI scheme 标识，约定 interview://<kind>/<name>
type resource struct {
    uri, name, mimeType, text string
}

case "resources/list": // 只返回清单：URI + 名称 + MIME 类型，不携带正文
case "resources/read": // 按 uri 查库，返回 {contents: [{uri, mimeType, text}]}
```

两个设计要点值得说透。一是 URI 用自定义 scheme（`interview://...`）而不是 http 链接——MCP 允许任意 URI 方案，自定义 scheme 明确表示「这个资源由本 server 私有」，不会和外部 URL 混淆。二是 `resources/list` 刻意不携带正文：清单可能很长，全部内联会让每次握手都背上全部资料的体积，按 URI 现取才符合「按需加载」的思路。`mimeType` 也不要省，宿主据此决定渲染方式——`text/markdown` 和 `text/plain` 在客户端的展示效果完全不同。

两者怎么选？一张表记住：

| | Tools | Resources |
|---|---|---|
| 语义 | 做一件事（计算、查询、发送） | 读一份资料（文档、题库、配置） |
| 谁触发 | 大模型自主决定调用，可能带来副作用 | 宿主或模型按 URI 显式读取，只读 |
| 典型入参 | 结构化 arguments | URI |
| 本项目对应物 | `query_question` 筛选题目 | 评分 Rubric、完整题库 Markdown |

而且两者不是割裂的：`query_question` 的背后就是题库这份「资源」——**工具负责加工，资源负责供给**，这正是下一篇要把面试教练的 SQLite 历史库包装成 MCP 服务的思路雏形。

## 五、实测：四个请求走完全程

对着 `mcp/toolsdemo` 起进程，按真实顺序发四个请求，响应全部符合预期：

1. `initialize` → `capabilities` 里如实声明了 `tools` 和 `resources` 两项（上一版这里是空的 `{}`）；
2. `tools/list` → 返回 calc、list_categories、query_question 三个工具，schema 完整；
3. `resources/list` → 返回两条 Markdown 资料，URI 分别为 `interview://bank/golang-concurrency` 和 `interview://rubric/go-backend`；
4. `tools/call`（calc 除零）→ 合法 JSON-RPC 响应 + `isError: true` + 中文错误文本。

加上单元测试和端到端测试一共 14 个用例全绿。测试分两层：分发器是纯函数，直接单测（含「除零必须是 isError 而不是协议错误」这类契约断言）；另起一个 `go run` 真进程走 stdio 管道做端到端，模拟宿主发四连请求，验证通知不产生响应、tools 数量为 3、resources 数量为 2。前者保证逻辑对，后者保证协议对，两层都有才算数。

到目前为止，我们没有引入任何 MCP SDK，协议的全部行为都在自己手里——理解了这层，再去用官方 SDK 或 mark3labs/mcp 这类社区库，你看懂的就不只是 API，而是它为什么要这样设计。

**下篇预告**：把我们的 AI 面试教练的 SQLite 历史库包装成 MCP 服务——`list_sessions`、`get_report` 两个工具直接复用 `internal/store`，面试官 Agent 从此「记得」每一位候选人。没看过第一系列也不影响，下篇会把需要的背景补全。

觉得有用的话，欢迎点赞、在看、转发，关注「Go2AI之路」，后面还有 5 篇 MCP 实战在路上。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
