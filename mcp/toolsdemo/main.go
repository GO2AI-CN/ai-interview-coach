// mcp/toolsdemo —— 进阶版 MCP Server：多个带业务逻辑的工具 + Resources 资料库。
//
// 相比 mcp/minimal 只演示协议骨架，本篇关注真实工程问题：
//   - 工具注册表：tools/list 不再硬编码，而是遍历注册表自动生成；
//   - 参数校验：模型的 arguments 不可信，必须逐字段校验；
//   - 错误分层：JSON-RPC error 表示「协议层失败」，isError=true 表示「业务执行失败」；
//   - Resources：resources/list + resources/read，把只读资料（题库、评分 rubric）
//     以 URI 形式暴露给宿主，模型需要背景知识时由宿主代为读取。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// protocolVersion 与 mcp/minimal 保持一致，直接手写字符串以强调：它就是一次握手交换的文本。
const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
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

// toolDef 描述一个可注册的工具。inputSchema 直接存 JSON Schema（map），
// tools/list 时原样返回给宿主，最终会被塞进大模型的 system prompt。
type toolDef struct {
	description string
	inputSchema map[string]any
	handler     func(args json.RawMessage) (string, error)
}

// registry 是工具注册表：name -> 定义。新增工具只需在此登记，tools/list 自动收录。
var registry = map[string]toolDef{}

func init() {
	registry["calc"] = toolDef{
		description: "四则计算器。op 支持 + - * /，除数为 0 会返回业务错误",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a":  map[string]any{"type": "number", "description": "左操作数"},
				"b":  map[string]any{"type": "number", "description": "右操作数"},
				"op": map[string]any{"type": "string", "enum": []string{"+", "-", "*", "/"}, "description": "运算符"},
			},
			"required": []string{"a", "b", "op"},
		},
		handler: handleCalc,
	}
	registry["query_question"] = toolDef{
		description: "按方向和难度筛选面试题，返回题目清单文本",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category":   map[string]any{"type": "string", "description": "方向，如 golang、system-design；缺省查全部"},
				"difficulty": map[string]any{"type": "string", "enum": []string{"easy", "medium", "hard"}, "description": "难度；缺省不限"},
			},
		},
		handler: handleQueryQuestion,
	}
	registry["list_categories"] = toolDef{
		description: "列出题库中所有可用的面试方向",
		inputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		handler: func(json.RawMessage) (string, error) { return strings.Join(categories(), ", "), nil },
	}
}

// question 是内存题库的一条记录。真实项目里它来自数据库（如面试教练的 SQLite），
// 这里用内存数据把「资源 → 工具」的关系演示清楚即可。
type question struct {
	category   string
	difficulty string
	text       string
}

var questionBank = []question{
	{"golang", "easy", "goroutine 和线程有什么区别？"},
	{"golang", "medium", "channel 关闭后读会怎样？如何优雅地通知所有消费者退出？"},
	{"golang", "hard", "请设计一个带超时与熔断的 worker pool，并说明各参数如何调。"},
	{"system-design", "medium", "设计一个短链接服务，QPS 10 万量级，如何分库分表？"},
	{"system-design", "hard", "设计一个支持百万在线的 IM 消息系统，如何保证消息有序与必达？"},
}

func categories() []string {
	set := map[string]bool{}
	for _, q := range questionBank {
		set[q.category] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// handleCalc 演示「参数校验 + 业务错误」。模型给出的 arguments 不保证合法，
// 每个字段都要校验；除零属于业务失败，应返回 isError 而不是 JSON-RPC error。
func handleCalc(args json.RawMessage) (string, error) {
	var in struct {
		A  *float64 `json:"a"`
		B  *float64 `json:"b"`
		Op string   `json:"op"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("arguments 不是合法 JSON: %w", err)
	}
	if in.A == nil || in.B == nil {
		return "", fmt.Errorf("a 和 b 都是必填的数字")
	}
	switch in.Op {
	case "+":
		return fmt.Sprintf("%v", *in.A+*in.B), nil
	case "-":
		return fmt.Sprintf("%v", *in.A-*in.B), nil
	case "*":
		return fmt.Sprintf("%v", *in.A**in.B), nil
	case "/":
		if *in.B == 0 {
			// 业务错误：请求格式完全合法，但语义上不能执行。
			return "", fmt.Errorf("除数不能为 0")
		}
		return fmt.Sprintf("%v", *in.A / *in.B), nil
	}
	return "", fmt.Errorf("不支持的运算符 %q，仅支持 + - * /", in.Op)
}

// handleQueryQuestion 演示「资源驱动的工具」：工具不拥有数据，
// 只是对底层资源（这里是内存题库，下篇换成 SQLite）做一次只读查询。
func handleQueryQuestion(args json.RawMessage) (string, error) {
	var in struct {
		Category   string `json:"category"`
		Difficulty string `json:"difficulty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("arguments 不是合法 JSON: %w", err)
	}
	var lines []string
	for _, q := range questionBank {
		if in.Category != "" && q.category != in.Category {
			continue
		}
		if in.Difficulty != "" && q.difficulty != in.Difficulty {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s/%s] %s", q.category, q.difficulty, q.text))
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("没有匹配的题目，可用方向: %s", strings.Join(categories(), ", "))
	}
	return strings.Join(lines, "\n"), nil
}

// resource 是 Resources 协议暴露的一条只读资料。URI 用自定义 scheme，
// 约定 interview://<kind>/<name>。
type resource struct {
	uri      string
	name     string
	mimeType string
	text     string
}

// resources 是静态资料库。真实场景中这些文本可能由 Markdown 文件或数据库视图生成。
var resources = []resource{
	{
		uri:      "interview://bank/golang-concurrency",
		name:     "Go 并发面试题库（Markdown）",
		mimeType: "text/markdown",
		text: "# Go 并发面试题库\n\n" +
			"1. goroutine 的调度模型是什么？（GMP）\n" +
			"2. channel 的 happens-before 语义如何保证？\n" +
			"3. sync.Pool 适合什么场景，有什么坑？\n",
	},
	{
		uri:      "interview://rubric/go-backend",
		name:     "Go 后端工程师评分 Rubric（Markdown）",
		mimeType: "text/markdown",
		text: "# Go 后端评分维度\n\n" +
			"| 维度 | 权重 | 考察点 |\n" +
			"| --- | --- | --- |\n" +
			"| 语言功底 | 30% | 并发模型、内存管理、接口设计 |\n" +
			"| 系统设计 | 30% | 容量估算、取舍、演进 |\n" +
			"| 工程素养 | 20% | 测试、可观测性、发布 |\n" +
			"| 沟通表达 | 20% | 结构化、边界澄清 |\n",
	},
}

func findResource(uri string) *resource {
	for i := range resources {
		if resources[i].uri == uri {
			return &resources[i]
		}
	}
	return nil
}

// handleRequest 是服务端分发器。相比 minimal 版，新增 resources/list 与 resources/read，
// 并在 initialize 的 capabilities 里如实声明两项能力。
func handleRequest(req *rpcRequest) *rpcResponse {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // 通知：处理但不回包
	}

	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "toolsdemo-mcp-server",
				"version": "0.2.0",
			},
		}

	case "ping":
		resp.Result = map[string]any{}

	case "tools/list":
		// 遍历注册表生成清单：新增工具不用改这里。
		tools := make([]any, 0, len(registry))
		for name, def := range registry {
			tools = append(tools, map[string]any{
				"name":        name,
				"description": def.description,
				"inputSchema": def.inputSchema,
			})
		}
		sort.Slice(tools, func(i, j int) bool { // 排序只为输出稳定，方便测试断言
			return tools[i].(map[string]any)["name"].(string) < tools[j].(map[string]any)["name"].(string)
		})
		resp.Result = map[string]any{"tools": tools}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		def, ok := registry[params.Name]
		if !ok {
			resp.Error = &rpcError{Code: -32601, Message: "unknown tool: " + params.Name}
			return resp
		}
		text, err := def.handler(params.Arguments)
		if err != nil {
			// 关键区分：handler 返回错误 = 业务执行失败。
			// 响应仍是合法 JSON-RPC（没有 error 字段），但 isError=true，
			// 宿主会把 content 里的错误文本展示给模型，让它自己决定怎么办。
			resp.Result = map[string]any{
				"isError": true,
				"content": []any{
					map[string]any{"type": "text", "text": err.Error()},
				},
			}
			return resp
		}
		resp.Result = map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
		}

	case "resources/list":
		list := make([]any, 0, len(resources))
		for _, r := range resources {
			list = append(list, map[string]any{
				"uri":      r.uri,
				"name":     r.name,
				"mimeType": r.mimeType,
			})
		}
		resp.Result = map[string]any{"resources": list}

	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.URI == "" {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: uri is required"}
			return resp
		}
		r := findResource(params.URI)
		if r == nil {
			resp.Error = &rpcError{Code: -32602, Message: "unknown resource uri: " + params.URI}
			return resp
		}
		resp.Result = map[string]any{
			"contents": []any{
				map[string]any{
					"uri":      r.uri,
					"mimeType": r.mimeType,
					"text":     r.text,
				},
			},
		}

	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: nil,
				Error: &rpcError{Code: -32700, Message: "parse error"}})
			fmt.Println(string(out))
			continue
		}

		resp := handleRequest(&req)
		if resp == nil { // 通知：不回包
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
