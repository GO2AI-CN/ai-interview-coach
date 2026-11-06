# MCP 收官篇：官方 SDK、社区库与服务器注册表全景

系列写到这里，我们已经手写了一个能跑通全流程的 MCP 实现：200 行的最小 server、面试历史库的 coachstore、让模型自己决定调工具的 client 循环、甚至包括 Streamable HTTP 传输和鉴权。但回到真实世界要问一句：**生产环境里，没人手写协议栈**。这篇收官就来补齐最后一块拼图——把前面八篇手写的东西，对接到真实的 MCP 生态：官方 SDK、社区库和服务器注册表。

## 一、手写之后，为什么还要 SDK

先回答一个必然的问题：既然八篇都跑通了，为什么还要学 SDK？

手写的价值在于心智模型。第 5 篇的工具调用循环、第 7 篇的传输层分拣逻辑，看懂这些之后，任何 SDK 的源码对你来说都只是「熟悉的协议加了层壳」。但生产的账单是另一回事：**协议在演进**（2025-06-18 → 2025-11-25 → 2026-07-28，我们的系列钉在最稳定的 2025-06-18），边界情况在堆积——分页游标、并发取消、SSE 断线重连、handler panic 恢复、OAuth 流程，mcp-go 仅 v1.5.0 一个版本就修了几十处这类边界问题。这些坑你不必亲自再踩一遍。

一句话：**学习时手写，生产时用 SDK。**

## 二、官方 Go SDK：schema 从类型里长出来

`github.com/modelcontextprotocol/go-sdk` 是 MCP 官方与 Google 合作维护的 Go SDK，拆成 mcp、jsonrpc、auth、oauthex 四个包。最新稳定版 **v1.7.0**，兼容协议 2026-07-28、2025-11-25、2025-06-18、2025-03-26、2024-11-05——以后协议升级，升级 SDK 就行。它的核心设计是**工具 schema 从 handler 的参数类型自动推导**，不用手维护一份 JSON：

```go
type ScoreInput struct {
	QuestionID string `json:"question_id" jsonschema:"题目 ID"`
	Answer     string `json:"answer"      jsonschema:"候选人作答"`
}
type ScoreOutput struct {
	Score int `json:"score" jsonschema:"评分，0-100"`
}

// handler 签名固定：ctx、原始请求、反序列化后的 typed input
func ScoreAnswer(ctx context.Context, req *mcp.CallToolRequest, in ScoreInput) (
	*mcp.CallToolResult, ScoreOutput, error) {
	return nil, ScoreOutput{Score: evaluate(in)}, nil // result 信封 SDK 自动补
}

server := mcp.NewServer(&mcp.Implementation{Name: "coach", Version: "v0.1.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "score_answer", Description: "评估作答"}, ScoreAnswer)
err := server.Run(ctx, &mcp.StdioTransport{}) // 阻塞运行，client 断开才返回
```

对比一下第 3 篇手写 server 时为每个工具手写 `inputSchema` JSON 的痛苦——这里的 `jsonschema` tag 就是文档，类型即契约，改结构体就自动改协议。

## 三、社区旗舰 mcp-go：功能全家桶

另一个绕不开的库是 `github.com/mark3labs/mcp-go`，Go 生态里最流行的 MCP 库。最新版 **v1.5.0**（2026-10-30 发布），README 明确写着实现 2025-11-25 规范，向后兼容 2025-06-18、2025-03-26、2024-11-05（v1.0.0 起还支持 2026-07-28）。风格比官方 SDK 更「框架化」，参数用链式 API 声明，还自带带校验的取值助手：

```go
s := server.NewMCPServer("coach", "v0.1.0", server.WithToolCapabilities(false))
s.AddTool(
	mcp.NewTool("score_answer",
		mcp.WithDescription("评估候选人作答"),
		mcp.WithString("answer", mcp.Required())), // schema 由链式声明生成
	func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		answer, err := req.RequireString("answer") // 缺参/错类型直接返回协议级错误
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(evaluate(answer)), nil
	})
err := server.ServeStdio(s) // 一行跑起 stdio server
```

一个真实的踩坑点值得说：v1.5.0 的 `ToolHandlerFunc` 收的是 `CallToolRequest` **值类型**而非指针，习惯写 `*mcp.CallToolRequest` 会直接编译报错。两个库怎么选？看下面的对照表。

## 四、服务器注册表：写完的 server 怎么被发现

协议和 SDK 解决「怎么连」，注册表解决「怎么被找到」。官方 MCP Registry 于 **2025-09-08** 以预览版上线（registry.modelcontextprotocol.io），核心是一个 `server.json` 标准格式：用 name、description、packages（npm/pypi 等安装方式）、remote endpoints（远程服务的 URL 和传输类型）描述你的 server。

发布方式有个反直觉的设计：**不是给 registry 仓库提 PR**，而是用官方的 `mcp-publisher` CLI 从你自己项目的仓库发布——server.json 归你维护，registry 只存索引。整个 registry 服务本身是开源的，任何平台都可以拉数据建自己的子注册表。回到我们的面试教练：哪天把 coachstore 整理成对外服务，写一份 server.json 发布上去，Claude Desktop、Cursor 这类客户端的用户就能直接搜到并接入——本地工具一步变成生态公共品。

## 五、手写 vs 官方 SDK vs mcp-go：选型对照表

| 维度 | 手写（本系列） | 官方 go-sdk v1.7.0 | mcp-go v1.5.0 |
|---|---|---|---|
| 学习价值 | ★★★ 协议内化 | ★★ 类型即协议 | ★★ 框架化快 |
| schema 维护 | 手写 JSON | struct tag 自动推导 | NewTool 链式声明 |
| 传输层 | 自己实现 | stdio / Streamable HTTP | stdio / SSE / HTTP / OAuth 全家桶 |
| 协议跟进 | 自己背 | 升级即跟进（已到 2026-07-28） | 升级即跟进（实现 2025-11-25） |
| 适合场景 | 学习、极窄定制 | 追新协议、要强类型 | 生产快速落地、要全家桶 |

## 六、两个系列总复盘：一张 Agent 开发路线图

至此两个系列 16 篇全部收官。把地图摊开看：

| 系列 | 主线 | 你带走了什么 |
|---|---|---|
| AI 模拟面试教练（8 篇） | 出题→追问→打分的产品级 Agent | 提示词工程、结构化输出、多轮对话状态机、渠道接入（Web/飞书/语音）、部署与持久化 |
| MCP 工程化（8 篇） | 从手写协议到接入生态 | JSON-RPC 与生命周期、Tools/Resources/Prompts、client 工具调用循环、远程传输与鉴权、SDK 与注册表 |

两条线其实是一条：**Agent = 模型 + 上下文工程 + 工具协议 + 评测闭环**。面试教练是「产品侧」的完整闭环，MCP 是「工具侧」的标准答案——而第 5 篇我们正是用自写的 coachclient 调通了自写的 coachstore，两个系列在代码里合过流。下一步给你三个方向：把 coachclient 换成官方 SDK 重写一遍（半天工作量，体验类型安全的爽感）；给 coachstore 写 server.json 发布到 registry；或者迎接下一个系列——RAG 与向量检索。

MCP 系列到此完结。觉得这套「手写到底再回生态」的学法有用，欢迎点赞、在看、转发，关注「GO语言入门之路」，评论区聊聊你想看哪个方向。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
