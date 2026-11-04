# 远程 MCP 实战：HTTP/SSE 传输与鉴权全解析

stdio 有个天然天花板：server 只能是 client 的子进程，只能活在同一台机器上。想让团队共用一个面试历史服务、想让 Claude Desktop 之外的手机端也能访问，就得把 MCP 搬上网络。这就是 Streamable HTTP 传输——2025-06-18 版规范里与 stdio 并列的第二种标准传输，也是本篇的主角。

先交代版本立场：官方规范线已经走到 2025-11-25 和更新的 2026-07-28，本系列所有实现钉在 **2025-06-18** 稳定版（理由前几篇说过：生态兼容性最成熟），传输层的核心机制各版本一致。

## 一、一个端点，两种方法

和旧版 HTTP+SSE（2024-11-05 协议的双端点方案，已废弃）不同，Streamable HTTP 只有**一个 MCP 端点**，靠 HTTP 方法区分方向：

- **POST**：client→server，每条 JSON-RPC 消息都是一次独立 POST；
- **GET**：client 挂起一条 SSE 长连接，server 主动推送（通知、反向请求）走这里。

server 端的分拣逻辑直接长在 Handler 里：

```go
func (s *streamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if !s.checkAuth(w, r) || !checkOrigin(w, r) || !checkProtocolVersion(w, r) {
        return // 三道闸：鉴权、Origin、协议版本头
    }
    switch r.Method {
    case http.MethodPost:
        s.handlePost(w, r)   // client→server 消息
    case http.MethodGet:
        s.handleGet(w, r)    // 挂 SSE 收推送
    case http.MethodDelete:
        s.handleDelete(w, r) // 显式终止会话
    }
}
```

POST 内部的分类也有讲究：通知和响应类消息收下入队、回 **202 Accepted** 无 body；请求类消息正常分发。这个 202 是规范明文要求的，漏了它，严谨 client 会以为消息丢了。

## 二、响应协商：一条请求，两种答法

client 的 POST 必须在 `Accept` 里同时声明 `application/json` 和 `text/event-stream`。server 据此二选一：单体 JSON，或 SSE 流——**流里可以先推若干无关消息，最后才带上本次请求的响应**。所以 client 的 SSE 读取器不能傻等第一行，要按事件逐条分拣、按 id 对号：

```go
case line == "": // 一条 SSE 事件收齐
    var msg struct{ ID json.RawMessage }
    if err := json.Unmarshal([]byte(raw), &msg); err != nil {
        continue // server 推的通知没有 id，跳过
    }
    if string(msg.ID) == strconv.Itoa(wantID) {
        return json.Unmarshal([]byte(raw), out) // 等到自己的响应，收工
    }
```

我们让 server 在收到 `Accept: ...text/event-stream` 时用 SSE 包裹响应（`event: message\ndata: {...}`），单发即关流——规范允许响应到达后关闭，重连靠 GET 流恢复。

GET 流这边有两个容易踩的坑。一是**心跳**：长连接挂着不动会被中间代理掐断，我们的实现每 25 秒写一条 SSE 注释行（冒号开头）当 keepalive。二是**推送不能阻塞主流程**：client 此刻不一定在收，`handleCall` 里往流上写消息用了 `select` + `default`，写不进去就丢——规范允许 server 在 stream 关闭时丢弃非关键通知，宁可丢也不能把工具调用卡死。

## 三、会话与协议版本头

HTTP 无状态，但 MCP 会话有状态（初始化协商结果、订阅关系）。规范给了三个明确的钩子：

- initialize 响应带 `Mcp-Session-Id` 头，client **之后每个请求都必须带**；
- server 要求会话而请求没带 → **400**；会话已被终止还硬带 → **404**（client 收到 404 必须重新握手）；
- client 不再需要会话时发 **DELETE** 显式终止。

```go
id := r.Header.Get("Mcp-Session-Id")
if id == "" {
    http.Error(w, `{"error":"Mcp-Session-Id required"}`, 400)
    return nil
}
// 查表：未知会话 404——这正是 DELETE 之后旧会话的命运
```

另有一个容易忽略的强制头：`MCP-Protocol-Version`。initialize 之后每个请求都要带协商出的版本号，带了不支持的版本必须回 400。我们的实现只认 `2025-06-18`，多一个字符都不行——这正是「钉死版本」的工程化表达。

DELETE 不只是「礼貌退出」：服务端会话里可能挂着资源（订阅关系、挂起的 GET 流），不主动终止就是泄露。我们 client 的 `Close` 里固定发 DELETE，server 收到即从会话表删除并关流——后面测试里「DELETE 之后再带旧会话 id 会被 404」正是验证这条链路闭环。

## 四、安全三件套与上线自检清单

规范对远程传输有一段罕见的加粗警告：不做好防护，攻击者可以用 DNS rebinding 通过浏览器网页操控你本机的 MCP server。三条对应的防线：**校验 Origin 头**（只放行本机来源）、**只绑 127.0.0.1**（本地服务）、**全连接鉴权**（Bearer token，常量时间比较防时序侧信道）。

| 自检项 | 规范依据 | 我们的实现 |
|---|---|---|
| 无/错 token → 401 + WWW-Authenticate | 规范 SHOULD 鉴权 | `subtle.ConstantTimeCompare` |
| 跨站 Origin → 403 | 规范 MUST 校验 Origin | 只放行 localhost/127.0.0.1 |
| 无会话 → 400 / 死会话 → 404 | 会话管理章节 | `checkSession` |
| DELETE 终止会话 | 会话管理章节 | `handleDelete` |
| 通知 POST → 202 无 body | 发送消息章节 | `handlePost` |
| 不支持版本 → 400 | 协议版本头章节 | `checkProtocolVersion` |
| 响应按 Accept 协商 JSON/SSE | 发送消息章节 | `writeResult` |

## 五、实测：全链路跑通

demo 二进制跑在 `127.0.0.1:8765`，内置 client 完成「握手→工具调用→通知→关闭会话」四步：

```
tools/call 结果: {"content":[{"text":"你好, 远程世界！远程 MCP 链路已通。","type":"text"}]}
握手→调用→通知→关闭会话：全链路通过
```

测试 10 个用例全绿，其中两个最见功夫：`TestGetStreamReceivesPush` 先在 GET 上挂 SSE 流，再触发 `tools/call`，断言 server 主动推的 `notifications/tools/called` 出现在流上——server→client 方向真实打通；`TestPostSSENegotiation` 验证 POST 的 SSE 协商路径。加上鉴权 401、Origin 403、会话 400/404 这几个负向用例，规范里加粗警告的三道防线都有对应的自动化测试守着，重构传输层时不会悄悄破防。至此，同一个 JSON-RPC 消息层，我们已经用两种传输（stdio / Streamable HTTP）各跑通了一遍——**协议与传输解耦**这四个字，从概念变成了可运行的代码。

下一篇收官：接真实 MCP 生态——官方 SDK、社区库（如 mark3labs/mcp）、服务器注册表，以及把两个系列（面试教练 × MCP 工程化）串成一张完整的 Agent 开发路线图。

觉得有用的话，欢迎点赞、在看、转发，关注「GO语言入门之路」，收官篇见。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
