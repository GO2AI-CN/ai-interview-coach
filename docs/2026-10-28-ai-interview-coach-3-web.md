# AI 面试官网页版实战：Go + SSE 流式输出（三）

上篇和中篇我们把 AI 模拟面试教练的核心跑通了：出题、追问、打分，还能读简历。但有个很现实的问题——**没人愿意在黑框框里面试**。

这个系列既然是奔着"产品级 Agent"去的，那就得有个产品该有的样子。这一篇给面试官装上界面：一个 `coach web` 命令启动网页版，浏览器里答题，点评逐字蹦出来。全程不引入任何前端框架和 WebSocket，只用 Go 标准库 + SSE。

## 一、技术选型：为什么是 SSE 不是 WebSocket

流式输出是 AI 产品的标配（想想 ChatGPT 逐字打印的效果）。但"流"的实现方式有好几种，选型先想清楚：

| 方案 | 协议 | 适用场景 | 本项目的取舍 |
|------|------|---------|-------------|
| SSE | HTTP 长连接，服务端单向推 | 服务端持续吐数据（AI 生成） | ✅ 正合适：点评是单向流 |
| WebSocket | 双工长连接 | 双向高频交互（聊天室、游戏） | 杀鸡用牛刀，还要自己处理心跳 |
| 轮询 | 客户端定时拉 | 实时性要求低 | 伪流式，体验差 |

SSE 的核心优势：**就是普通 HTTP**。复用现有的路由、鉴权、超时控制，Nginx 反向代理也不用特殊配置；浏览器原生 `EventSource` 支持断线重连。AI 场景的数据流几乎全是"服务端 → 客户端"单向的，SSE 完全够用。

## 二、整体架构：一个 embed 的 HTML + 5 个接口

工程上零依赖新增——页面用 `go:embed` 打进二进制，接口用 Go 1.22 的 `http.ServeMux` 方法模式路由：

```text
GET  /              面试页面（embed 的 index.html）
POST /api/start     开始面试：生成计划，返回首题
GET  /api/answer    SSE：提交回答，流式返回面试官点评
GET  /api/question  取下一题
GET  /api/report    取最终评分报告（Markdown）
```

会话状态放在内存 `map[string]*Interview` 里，单机单用户足够。一个要刻意强调的设计决策：**评估仍然走 JSON 模式非流式，只有点评文案走流式**——结构化数据要求格式严格，宁可等 2 秒一次性拿到合法 JSON，也不能流式 parse 半拉子结构。

## 三、服务端：20 行写出 SSE 流

openai-go v3 的流式调用返回 `ssestream.Stream`，`Next()/Current()` 逐 chunk 读取，每个 chunk 的 `Delta.Content` 就是增量文本：

```go
// internal/llm/client.go — ChatStream 边读边回调
func (c *OpenAIClient) ChatStream(ctx context.Context, system, user string,
    onToken func(string)) (string, error) {
    stream := c.cli.Chat.Completions.NewStreaming(ctx, c.chatParams(system, user))
    defer stream.Close()
    var full strings.Builder
    for stream.Next() {
        for _, choice := range stream.Current().Choices {
            if tok := choice.Delta.Content; tok != "" {
                full.WriteString(tok)
                onToken(tok) // 逐 token 回调给 HTTP 层
            }
        }
    }
    return full.String(), stream.Err()
}
```

HTTP 层把每个 token 包成 SSE 事件推给浏览器，`http.Flusher` 立即冲刷：

```go
// internal/web/server.go — SSE 事件发送
w.Header().Set("Content-Type", "text/event-stream")
flusher := w.(http.Flusher)
send := func(event string, data any) {
    b, _ := json.Marshal(data)
    fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
    flusher.Flush() // 立即推送，不缓冲
}
_, _ = s.client.ChatStream(ctx, sys, commentary, func(tok string) {
    send("token", map[string]string{"t": tok})
})
send("result", result) // 追问 or 下一题 or 结束
send("done", map[string]bool{})
```

SSE 协议本身就两个约定：每个事件以 `event:` + `data:` 行组成、空行分隔。没有库也不神秘。

## 四、前端：fetch 读流，手写事件解析

这里有个实用细节：回答内容通过 URL 参数传给 `/api/answer`，而原生 `EventSource` 只能发 GET 且不支持自定义 body，超长回答会被截断。所以前端用 `fetch` 读流、自己按空行切块解析——代码也就 20 行：

```javascript
// 按 SSE 空行分隔解析事件块
const resp = await fetch('/api/answer?session='+session+'&a='+encodeURIComponent(a));
const reader = resp.body.getReader(), dec = new TextDecoder();
let buf = '';
while (true) {
  const {done, value} = await reader.read(); if (done) break;
  buf += dec.decode(value, {stream: true});
  let p;
  while ((p = buf.indexOf('\n\n')) >= 0) {
    const block = buf.slice(0, p); buf = buf.slice(p + 2);
    handleEvent(block); // 解析 event: / data: 行
  }
}
```

`handleEvent` 里按事件类型分发：`token` 事件把字追加到点评气泡，`result` 事件决定是否追问/进下一题，`done` 关闭加载态。

## 五、踩坑清单

| 坑 | 现象 | 解法 |
|----|------|------|
| 忘了 Flush | SSE 连接建立但前端收不到字，最后一次性全到 | 每个事件后 `flusher.Flush()` |
| 响应被缓冲 | 反向代理攒数据才发 | `Cache-Control: no-cache` + 代理开 `X-Accel-Buffering: no` |
| 流式解析结构化数据 | 半个 JSON 就 parse 崩了 | 评估走 JSON 模式非流式，只有点评走流式 |
| EventSource 传参 | 长回答被 URL 截断 | 改用 fetch 读流手写解析 |
| 端口/会话错乱 | 多人共用一份状态 | 会话 ID + map 锁；单机版够用，上多人再加存储 |

## 六、跑起来

```bash
export OPENAI_API_KEY=你的Key
go run ./cmd/coach web --addr :8080
# 打开 http://localhost:8080
```

一个深色主题的聊天界面：输入岗位 → 逐题作答 → 点评像真人一样逐字打出来 → 结束出 Markdown 报告。完整代码（含测试）在 `https://github.com/LittleMoreInteresting/ai-interview-coach`，这篇的所有代码片段都来自真实提交，`go test ./...` 全绿。

## 七、总结

三篇下来，一个命令行 Agent 变成了有界面的产品：结构化数据走 JSON Schema 保正确，展示层走 SSE 保体验——两条通道各司其职，是这个项目里最重要的工程决策。

如果对你有帮助，欢迎点赞、在看、转发，关注「Go2AI之路」，下一篇聊聊给这个面试官接上语音，或者把它部署成一个真正对外服务的产品。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
