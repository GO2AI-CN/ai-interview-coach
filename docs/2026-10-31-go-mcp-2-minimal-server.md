# 不写一行 SDK，200 行 Go 手写 MCP Server 全解析

上一篇我们抓包看完了 MCP 握手的四条 JSON，有读者在评论区问：协议是懂了，但总感觉隔着一层——SDK 替我们做了什么？

这一篇就把这层窗户纸捅破：**不写一行第三方 SDK，用 200 行纯 Go 标准库实现一个完整可跑的 MCP Server**。写完你会发现，MCP Server 的门槛比写一个 HTTP 接口还低——因为它本质上就是个「stdin 读 JSON、stdout 写 JSON」的命令行程序。

全部代码位于系列仓库的 `mcp/minimal/` 目录（系列仓库地址见公众号置顶第一篇），文中附自检清单，建议收藏对照。

## 一、传输层：按行读写的 stdio 循环

MCP 标准传输之一就叫 stdio：宿主把 server 当子进程启动，server 从标准输入读请求、往标准输出写响应，**一行一个 JSON**。没有端口、没有 HTTP、没有 TLS。

核心就是一个 Scanner 循环：

```go
scanner := bufio.NewScanner(os.Stdin)
scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // MCP 消息可能携带大段参数

for scanner.Scan() {
    line := scanner.Bytes()
    if len(line) == 0 {
        continue
    }
    var req rpcRequest
    if err := json.Unmarshal(line, &req); err != nil {
        // 连 method 都没解析出来，只能返回 id 为 null 的顶层错误
        out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: nil,
            Error: &rpcError{Code: -32700, Message: "parse error"}})
        fmt.Println(string(out))
        continue
    }
    // 解析成功 → 交给分发器，见下文
}
```

两个容易忽略的细节：一是 Scanner 默认 64KB 行上限，工具参数一大就会截断，必须调 `Buffer`；二是解析失败时按 JSON-RPC 规范要回 `-32700 Parse error`，且此时 id 未知只能给 `null`——宿主靠 id 对账，这条错误它大概率对不上，但协议要求你必须发。

## 二、消息骨架：三个 struct 走天下

MCP 跑在 JSON-RPC 2.0 上，所有消息共享同一个骨架。Go 侧用 `json.RawMessage` 收 id 是关键技巧——因为 id 可能是数字也可能是字符串，且**通知根本没有 id**：

```go
// rpcRequest / rpcResponse 是 JSON-RPC 2.0 的最小骨架。
// MCP 的所有消息（请求、响应、通知）都长这个形状。
type rpcRequest struct {
    JSONRPC string          `json:"jsonrpc"`      // 固定 "2.0"
    ID      json.RawMessage `json:"id,omitempty"` // 通知（notification）没有 id
    Method  string          `json:"method"`       // 例如 "initialize"、"tools/call"
    Params  json.RawMessage `json:"params,omitempty"`
}
```

`Params` 也声明成 `RawMessage`：不同 method 的参数结构完全不同，先原样收下来，进分发器再按 method 二次解析。这是 Go 处理异构协议消息的惯用手法，LSP 的 SDK 内部也是这么做的。

## 三、分发器：四个 case 讲完协议主流程

`handleRequest` 就是个 switch，抽成纯函数是为了不写文件、不起进程就能单测：

```go
switch req.Method {
case "initialize":
    // 握手：告诉宿主「我是谁、支持什么」
    resp.Result = map[string]any{
        "protocolVersion": protocolVersion,
        "capabilities":    map[string]any{},
        "serverInfo": map[string]any{
            "name":    "minimal-mcp-server",
            "version": "0.1.0",
        },
    }
case "ping":
    resp.Result = map[string]any{} // 保活探针，空结果即可
case "tools/list":
    // 声明工具清单，宿主会原样转述给大模型
case "tools/call":
    // 模型决定调用后，请求落在这里
default:
    resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}
```

协议版本号这里直接手写常量 `"2025-06-18"`——故意不引 SDK 常量，就是要看清握手第一步的本质：**双方交换的只是个字符串**，协商不成各自降级即可，没有任何魔法。

## 四、踩坑实录：通知消息差点让我对不上账

这一节是本文最值钱的部分，因为我们真的踩了坑。

`notifications/initialized` 这类通知**没有 id，协议要求处理但不许回包**。第一版代码我在主循环里过滤：

```go
resp := handleRequest(&req)
out, err := json.Marshal(resp)   // ← 问题在这
fmt.Println(string(out))
```

测试一跑，管道里冒出一条 `null`。原因很 Go：`handleRequest` 对通知返回 `nil`，而 `json.Marshal(nil)` 的结果是合法 JSON——字符串 `"null"`，不是空切片。于是通知也回了一条消息，宿主收到一条 id 对不上的「幽灵响应」。

修复方式是把「是否回包」的判断挪进分发器，返回 nil 就跳过：

```go
resp := handleRequest(&req)
if resp == nil { // 通知：已处理，按协议不回包
    continue
}
```

这类 bug 单元测试抓得住，真接 Claude Desktop 就是「server 时灵时不灵」的玄学问题。写协议代码，**先读规范里「什么时候不许说话」的条款**，和学会「怎么说话」同等重要。

## 五、收藏向：手写 MCP Server 自检清单

跑通最小实现后，接入真实宿主（Claude Desktop 等）前，对照这份清单逐项检查：

| # | 检查项 | 为什么重要 |
|---|---|---|
| 1 | initialize 返回 protocolVersion / capabilities / serverInfo | 缺字段直接握手失败 |
| 2 | 通知（无 id 消息）处理但不回包 | 幽灵响应会让宿主状态错乱 |
| 3 | Scanner 行缓冲调到 1MB 以上 | 大参数截断产生诡异 parse error |
| 4 | 未知 method 回 -32601，参数错回 -32602 | 标准错误码，宿主按码分类处理 |
| 5 | tools/list 的 inputSchema 必填 required | 模型靠它生成合法参数 |
| 6 | tools/call 返回 content 数组而非裸字符串 | 协议规定的统一返回结构 |
| 7 | 日志只写 stderr，stdout 只出协议消息 | 一行脏输出就毁掉整个 JSON 流 |
| 8 | 有端到端测试：起真进程、走管道收发 | 单测覆盖不了进程生命周期问题 |

第 7 条值得展开：stdio server 的 stdout 是数据通道，`fmt.Println("debug")` 这类调试习惯会把宿主打懵。要调试就 `fmt.Fprintln(os.Stderr, ...)`，宿主的 stderr 通常转发到它自己的日志里。

## 六、测试与下一篇预告

测试分两层：分发器是纯函数，直接表驱动单测；stdio 管道用一个端到端测试兜底——`go run` 起真进程，stdin 写入请求，断言 stdout 读回的 JSON：

```go
cmd := exec.Command("go", "run", ".")
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
cmd.Start()
stdin.Write([]byte(`{"jsonrpc":"2.0","id":9,"method":"ping"}` + "\n"))
// 读回一行，断言 id=9 且 error 为 nil
```

这套测试不依赖任何 API Key，clone 下来 `go test ./mcp/...` 全绿。

至此，MCP Server 的最小闭环已经在你手里了。但 echo 只是个玩具——下一篇《Tools 与 Resources：给 Agent 装上「手」和「资料库」》我们会让工具真正有用：给 server 挂上多个带业务逻辑的工具，并暴露只读资源，模型的「能力边界」就此打开。

---

对照清单跑通自己的最小 server 了吗？欢迎**点赞、在看、转发**，**关注「Go2AI之路」**，下篇带你给 Agent 装「手」。

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
