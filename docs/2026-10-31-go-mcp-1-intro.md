# 为什么大模型需要 MCP？一篇讲透 Agent 工具的 USB-C 接口

上一系列，我们用 Function Calling 加大模型造了一个能出题、会追问、自动打分的 AI 面试教练。做到第八篇你会发现一个尴尬的事实：Agent 的能力越强，手上挂的工具越多——查数据库、读简历、发飞书消息，每接一个工具，就要在代码里手写一遍「工具声明 JSON + 调用分发 + 结果回填」。

三家模型厂商三种格式，五个桌面应用五种配置。工具明明一样，对接却要重来。

MCP（Model Context Protocol）就是来终结这种重复的。这一篇讲清楚它解决什么问题、协议本质是什么，下一篇我们用 Go 手写一个最小 MCP Server——你会发现它远没有听起来高深。

## 一、Function Calling 治好了「模型不会调工具」，却留下三个痛点

先公平地说：Function Calling 本身是个伟大的发明。它让模型学会在对话中「举手」：我要调用 `query_interview_history`，参数是 `{"limit": 5}`。

但把它放进真实工程，三个痛点会反复出现：

**格式私有化。** 工具清单用 JSON Schema 描述，可「模型举手」的报文格式各家自定。你在 OpenAI 适配层写好的工具调用循环，换个国产兼容端点就要回归测试一遍——我们第七篇给 DeepSeek 做降级处理时深有体会。

**工具无法被发现。** Function Calling 的工具列表是你在代码里硬编码的。换一个宿主（桌面 Agent、IDE 插件、聊天软件），同一批工具要重新注册一遍。工具没有「插上就能用」的标准插座。

**上下文接入无协议。** 工具调用结果怎么回填对话、资源（文件、数据库记录）怎么暴露给模型，全靠各宿主自己发明轮子，行为不可预期。

一句话总结：Function Calling 解决了「单次怎么调」，没解决「工具怎么标准化地生产、发现和接入」。

## 二、MCP 是什么：给工具世界定一个 USB-C 接口

MCP 是 Anthropic 于 2024 年 11 月开源的开放协议，用一句话概括：**它规定了「宿主应用 ↔ 模型 ↔ 工具/数据源」之间的通信标准**，让任何工具只要实现一次 MCP，就能被所有支持 MCP 的宿主（Claude Desktop、Cursor、各类 Agent 框架，以及我们自己写的程序）直接调用。

类比最直观：

| | 标准化之前 | 标准化之后 |
|---|---|---|
| 手机充电 | 每家一根专用线 | USB-C，一线通吃 |
| 模型调工具 | 每个宿主手写对接 | MCP，一次实现处处可用 |
| 本质 | N×M 对接 | N+M 对接 |

架构上是经典的三方角色：

- **Host**（宿主）：用户真正打开的应用，比如 IDE、聊天窗口；
- **Client**：宿主内嵌的协议客户端，一个 server 对应一个 client 连接；
- **Server**：暴露能力的进程/服务，标准实现里它就是个读 stdin、写 stdout 的普通程序。

注意最后这点：**一个 MCP Server 本质上就是个黑白字符进出的进程**。这就是下一篇我们能用纯 Go、零依赖手写出来的原因。

## 三、握手实测：四条 JSON 看懂协议本质

空讲不如抓包。我们把下一篇要写的最小 server 提前跑起来，用管道模拟宿主发消息——完整的面试 handshake 长这样（真实输出，未删减字段）：

**第 1 条：initialize——互相报名字**

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-host","version":"0.0.1"}}}
{"jsonrpc":"2.0","id":1,"result":{"capabilities":{},"protocolVersion":"2025-06-18","serverInfo":{"name":"minimal-mcp-server","version":"0.1.0"}}}
```

**第 2 条：notifications/initialized——没有 id 的通知**

```json
{"jsonrpc":"2.0","method":"notifications/initialized"}
```

这条消息服务端**必须处理、绝不能回复**——JSON-RPC 里凡是没有 `id` 的就是通知，回复了反而会让宿主对不上账。这个细节是手写实现最容易踩的坑，我们下一篇的代码里专门用注释标了出来。

**第 3 条：tools/list——声明「我会干什么」**

```json
{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","description":"回显输入的文本……","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}}
```

宿主拿到这份清单后，会原样转述给大模型。也就是说：**模型的「工具视野」= server 在 tools/list 里声明的东西**。安全边界天然清晰——没声明的，模型根本看不见。

**第 4 条：tools/call——模型决定后的实际调用**

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"你好，MCP！"}}}
{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"你好，MCP！"}]}}
```

看明白了吗？**MCP 不神秘，就是带约定的 JSON-RPC 2.0**：一行一个 JSON，stdin 进、stdout 出，错误码沿用 -32601（方法不存在）、-32602（参数错误）这些标准编号。如果你写过 LSP（语言服务器协议）的对接，会有强烈的既视感——两者几乎是同一个设计哲学在不同领域的落地。

## 四、收藏向：MCP 三件套对照表

MCP 的能力不止工具。官方把 server 能暴露的东西分为三类，一张表记牢：

| 能力 | 方法前缀 | 干什么用 | 类比 |
|---|---|---|---|
| **Tools** | `tools/*` | 模型可调用的动作（查询、发送、计算） | 函数 |
| **Resources** | `resources/*` | 暴露给模型读的数据（文件、记录、日志） | 只读数据库视图 |
| **Prompts** | `prompts/*` | 预置的提示词模板，供用户/模型选用 | 代码片段 snippet |

三者权限模型也不同：Tools 是「模型主动调」，Resources 偏向「用户或模型按需读」，Prompts 则是「人先从菜单里挑」。判断某个能力该做成哪一类，就看触发它的是模型、用户还是两者皆可。

## 五、Go 开发者现在能做什么

协议是语言无关的，官方提供了多种语言的 SDK，Go 生态里也有社区实现。但我们的路线和上一系列一致：**先抛开 SDK 手写一遍，理解本质，再用轮子**。

下一篇《用 Go 手写 MCP Server：JSON-RPC 2.0 与 stdio 传输》会带你在 200 行以内实现一个完整可跑的 server：握手、通知处理、工具声明、工具调用，外加端到端测试。之后我们会把面试教练的 SQLite 历史库包装成 MCP 服务，让任何支持 MCP 的 Agent 都能直接查询面试报告——两个系列在此交汇。

工具的 USB-C 时代才刚刚开始，现在上车，正是时候。

---

如果觉得这篇讲透了 MCP 的本质，欢迎**点赞、在看、转发**给正在做 Agent 的同事，**关注「Go语言入门之路」**，下周二继续手写 MCP Server。

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
