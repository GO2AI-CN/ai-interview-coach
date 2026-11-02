// mcp/extras —— MCP 进阶能力演示：Prompts / Sampling / Roots（系列二第 6 篇配套）。
//
// 本篇演示三个「不是 Tools/Resources」的协议能力：
//   - Prompts：server 暴露提示词模板（prompts/list + prompts/get），
//     把团队打磨好的 prompt 当版本化资产分发；
//   - Sampling：server 在处理 tools/call 的过程中，反向请求 client 做一次模型补全
//     （sampling/createMessage）。这是全系列第一次出现「server 主动发请求」，
//     协议因此从一问一答变成双向对等；
//   - Roots：server 声明 roots 能力后，可向 client 发起 roots/list 查询，
//     拿到工作区目录边界。
//
// 立场声明（重要）：sampling 功能已于协议版本 2026-07-28 起被官方标记废弃
// （SEP-2577），新实现不应采用。本 demo 实现它仅为三件事：
// 理解旧 server 的行为、练手双向 JSON-RPC、看懂「为什么被废弃」
// （server 借 client 的模型配额绕开密钥管理，安全边界模糊）。
//
// 本目录协议形状以 2025-06-18 稳定版为准。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// protocolVersion 与前五篇保持一致。
const protocolVersion = "2025-06-18"

// askTimeout 是 server 反向请求 client 的等待上限。
// client 可能不支持该能力、可能弹窗等人审批——等不到就必须超时，
// 绝不能让一个 tools/call 挂死在读循环里。
const askTimeout = 10 * time.Second

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcWire 是双向通用消息骨架：同一结构既表示请求（有 method），
// 也表示响应（有 result/error 无 method）。
type rpcWire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}

// server 的 ask 字段是「反向请求」的注入点：
// 生产环境由 pending 通道 + 读循环驱动；测试里换成桩函数即可单测工具逻辑。
type server struct {
	out     io.Writer
	outMu   sync.Mutex
	nextID  int
	pendMu  sync.Mutex
	pending map[string]chan rpcWire
	ask     func(method string, params any) (json.RawMessage, error)
}

func newServer(out io.Writer) *server {
	s := &server{out: out, pending: map[string]chan rpcWire{}}
	s.ask = s.askReal
	return s
}

// askReal 发送一个 server→client 请求并等待同 id 响应（带超时）。
// 现在会被多个 dispatch goroutine 并发调用，id 自增需要加锁。
func (s *server) askReal(method string, params any) (json.RawMessage, error) {
	s.pendMu.Lock()
	s.nextID++ // 与 pending 共用一把锁：id 生成与注册原子完成
	id := fmt.Sprintf("srv-%d", s.nextID)

	ch := make(chan rpcWire, 1)
	// 注意：map 的 key 用 JSON 编码后的 id（带引号），
	// 读循环里 string(msg.ID) 拿到的正是这种形态——两边的 id 形态必须一致
	key := string(mustRaw(id))
	s.pending[key] = ch
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, key)
		s.pendMu.Unlock()
	}()

	p, _ := json.Marshal(params)
	// ID 上线前必须 JSON 编码（字符串要带引号）——RawMessage 是逐字节透传的，
	// 直接塞 "srv-1" 会产出非法 JSON，marshal 直接失败、写出一行空行
	line, err := json.Marshal(rpcWire{JSONRPC: "2.0", ID: mustRaw(id),
		Method: method, Params: p})
	if err != nil {
		return nil, err
	}
	if err := s.writeLine(line); err != nil {
		return nil, err
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("client 拒绝: %s", msg.Error.Message)
		}
		return msg.Result, nil
	case <-time.After(askTimeout):
		return nil, errors.New("等待 client 响应超时（client 可能不支持该能力）")
	}
}

func (s *server) writeLine(line []byte) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, err := fmt.Fprintln(s.out, string(line))
	return err
}

// dispatch 处理 client→server 请求（有 method 有 id）。
func (s *server) dispatch(req *rpcWire) *rpcWire {
	resp := &rpcWire{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = mustRaw(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"prompts": map[string]any{"listChanged": false},
				"roots":   map[string]any{},
			},
			"serverInfo": map[string]any{"name": "extras-mcp-server", "version": "0.4.0"},
		})

	case "ping":
		resp.Result = mustRaw(map[string]any{})

	case "prompts/list":
		resp.Result = mustRaw(map[string]any{
			"prompts": []any{
				map[string]any{
					"name":        "review_session",
					"description": "把一场面试的完整报告转成结构化复盘提示词",
					"arguments": []any{
						map[string]any{"name": "report_md", "description": "报告 Markdown 全文", "required": true},
					},
				},
			},
		})

	case "prompts/get":
		resp.Result = s.handlePromptsGet(req.Params, resp)

	case "tools/list":
		resp.Result = mustRaw(map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "polish_report",
					"description": "用一次模型补全把面试报告润色成口语化复盘（经 client 的 sampling 能力）",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"report_md": map[string]any{"type": "string", "description": "报告 Markdown 全文"},
						},
						"required": []string{"report_md"},
					},
				},
			},
		})

	case "tools/call":
		resp.Result = s.handleToolsCall(req.Params, resp)

	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// handlePromptsGet 演示「参数化模板」：模板存在 server 端，参数由 client 传入。
// 必填参数缺失按协议返回 -32602（Invalid params）。
func (s *server) handlePromptsGet(params json.RawMessage, resp *rpcWire) json.RawMessage {
	var in struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
		return nil
	}
	if in.Name != "review_session" {
		resp.Error = &rpcError{Code: -32602, Message: "unknown prompt: " + in.Name}
		return nil
	}
	report, ok := in.Arguments["report_md"]
	if !ok || report == "" {
		resp.Error = &rpcError{Code: -32602, Message: "缺少必填参数 report_md"}
		return nil
	}
	text := "你是一位面试复盘教练。请基于下面的评分报告输出三段复盘：\n" +
		"1. 本场最强的能力维度及证据；2. 最弱的维度与具体失分点；3. 下一次面试前的一周三步走计划。\n\n" +
		"报告全文：\n" + report
	return mustRaw(map[string]any{
		"messages": []any{
			map[string]any{"role": "user",
				"content": map[string]any{"type": "text", "text": text}},
		},
	})
}

// handleToolsCall 里的 polish_report 是本篇核心：处理 client 请求的过程中，
// server 反向发起 sampling/createMessage，把「用什么模型、带不带 key」
// 的决定权交还 client——这正是 sampling 被废弃前想解决的问题。
func (s *server) handleToolsCall(params json.RawMessage, resp *rpcWire) json.RawMessage {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Name != "polish_report" {
		resp.Error = &rpcError{Code: -32601, Message: "unknown tool"}
		return nil
	}
	var args struct {
		ReportMD string `json:"report_md"`
	}
	if err := json.Unmarshal(in.Arguments, &args); err != nil || args.ReportMD == "" {
		resp.Error = &rpcError{Code: -32602, Message: "report_md is required"}
		return nil
	}

	res, err := s.ask("sampling/createMessage", map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": map[string]any{
				"type": "text",
				"text": "把下面的面试报告润色成 100 字以内的口语化复盘总结：\n" + args.ReportMD,
			}},
		},
		"maxTokens": 300,
	})
	if err != nil {
		// 业务错误（isError）：client 拒绝/超时是常态，把原因喂回模型层
		return mustRaw(map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text", "text": "采样失败：" + err.Error()}},
		})
	}

	var sample struct {
		Role    string `json:"role"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &sample); err != nil || sample.Content.Text == "" {
		return mustRaw(map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text", "text": "采样结果格式不符"}},
		})
	}
	return mustRaw(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": sample.Content.Text}},
	})
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// route 是读循环的分拣员：按「有没有 method / 有没有 id」把消息分到三条路。
func (s *server) route(line []byte) {
	var msg rpcWire
	if err := json.Unmarshal(line, &msg); err != nil {
		s.writeLine(mustRaw(map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "parse error"},
		}))
		return
	}

	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		// client→server 请求：分发可能阻塞（如等待采样回包），
		// 必须在独立 goroutine 里处理——读循环必须保持运转，
		// 否则 server 自己发出的反向请求永远等不到响应（自锁）。
		msg := msg
		go func() {
			resp := s.dispatch(&msg)
			line, _ := json.Marshal(resp)
			s.writeLine(line)
		}()

	case msg.Method != "": // 通知：initialized 后顺手查一次 roots（演示 roots 能力）
		if msg.Method == "notifications/initialized" {
			go s.queryRoots()
		}

	default: // 无 method 有 id = client 对 server 请求的响应：按 id 投递给等待者
		s.pendMu.Lock()
		ch, ok := s.pending[string(msg.ID)]
		s.pendMu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

// queryRoots 演示 roots：server 主动问 client「你的工作区边界是哪些目录」。
// 查不到不致命——roots 是优化信息，不是运行前提。
func (s *server) queryRoots() {
	res, err := s.ask("roots/list", map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[roots] 查询失败（可忽略）: %v\n", err)
		return
	}
	var roots struct {
		Roots []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(res, &roots); err != nil {
		fmt.Fprintf(os.Stderr, "[roots] 解析失败: %v\n", err)
		return
	}
	for _, r := range roots.Roots {
		fmt.Fprintf(os.Stderr, "[roots] 允许的工作区: %s (%s)\n", r.URI, r.Name)
	}
}

func main() {
	s := newServer(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		s.route(scanner.Bytes())
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "stdin error:", err)
		os.Exit(1)
	}
}
