// mcp/minimal —— 不依赖任何 SDK 的最小 MCP Server。
// 通过标准输入/输出以「一行一个 JSON」的 JSON-RPC 2.0 消息与宿主（Claude Desktop、
// 自研 Agent 等）通信，实现 initialize / ping / tools/list / tools/call 四个方法，
// 帮助理解 MCP 协议的本质：它并不神秘，就是带约定的 JSON-RPC。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// protocolVersion 是 MCP 协议版本号。这里手写字符串而非引入 SDK 常量，
// 正是为了演示：协议握手的第一步就是双方交换这个字符串。
const protocolVersion = "2025-06-18"

// rpcRequest / rpcResponse 是 JSON-RPC 2.0 的最小骨架。
// MCP 的所有消息（请求、响应、通知）都长这个形状。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`      // 固定 "2.0"
	ID      json.RawMessage `json:"id,omitempty"` // 通知（notification）没有 id
	Method  string          `json:"method"`       // 例如 "initialize"、"tools/call"
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleRequest 是服务端的核心分发器：每个进来的请求按 method 路由。
// 抽成纯函数是为了可以直接单测，不必起进程。
func handleRequest(req *rpcRequest) *rpcResponse {
	// 通知（notification）没有 id：按协议必须处理但不回复，
	// 否则宿主会以为收到了一条无法对账的响应。
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil
	}

	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		// 握手：告诉宿主「我是谁、支持什么」。capabilities 为空表示
		// 暂不提供 tools/resources 之外的高级能力。
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo": map[string]any{
				"name":    "minimal-mcp-server",
				"version": "0.1.0",
			},
		}

	case "ping":
		// 保活探针，原样返回空结果即可。
		resp.Result = map[string]any{}

	case "tools/list":
		// 声明本 server 提供的工具清单。宿主会把这里的结果
		// 原样塞进大模型的 system prompt，让模型「看见」工具。
		resp.Result = map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "回显输入的文本，用于验证 tools/call 链路是否打通",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{
								"type":        "string",
								"description": "要回显的文本",
							},
						},
						"required": []string{"text"},
					},
				},
			},
		}

	case "tools/call":
		// 模型决定调用工具后，宿主把请求转发到这里。params.name 是工具名，
		// params.arguments 是模型生成的参数（JSON）。
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		if params.Name != "echo" {
			resp.Error = &rpcError{Code: -32601, Message: "unknown tool: " + params.Name}
			return resp
		}
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil || args.Text == "" {
			resp.Error = &rpcError{Code: -32602, Message: "arguments.text is required"}
			return resp
		}
		// content 是 MCP 规定的返回结构：文本、图片、资源统一放在数组里。
		resp.Result = map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": args.Text},
			},
		}

	default:
		// JSON-RPC 标准错误码：-32601 = method not found。
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // MCP 消息可能携带大段参数

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// 连 method 都没解析出来，只能返回 id 为 null 的顶层错误。
			out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: nil,
				Error: &rpcError{Code: -32700, Message: "parse error"}})
			fmt.Println(string(out))
			continue
		}

		resp := handleRequest(&req)
		if resp == nil { // 通知：已处理，按协议不回包
			continue
		}
		out, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		fmt.Println(string(out))
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "stdin error:", err)
		os.Exit(1)
	}
}
