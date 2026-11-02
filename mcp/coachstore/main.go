// mcp/coachstore —— 把面试教练的 SQLite 历史库包装成 MCP 服务（上篇：只读查询工具）。
//
// 设计要点：
//   - 只读消费者：不 import internal/store，直接用 database/sql 查两张已知表
//     （interviews / answers，schema 见系列一第七篇）——schema 即契约，
//     MCP 服务与业务进程解耦，可以独立演进、独立部署；
//   - 双保险只读：PRAGMA query_only=ON，即使工具被模型乱调也写不坏历史库；
//   - busy_timeout=5000：面试教练进程可能正在写库，撞锁等待而不是立刻失败。
//
// 用法：COACH_DB_PATH=./coach.db go run ./mcp/coachstore
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动，纯 Go 无 CGO
)

// protocolVersion 与前两篇保持一致。
const protocolVersion = "2025-06-18"

// defaultLimit / maxLimit 约束 list_sessions 的返回条数，
// 工具结果会整个塞进模型上下文，无上限是自杀行为。
const (
	defaultLimit = 10
	maxLimit     = 50
)

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

// server 持有数据库句柄。handleRequest 是方法而非纯函数——这正是
// 本篇与 toolsdemo 的区别：工具背后有真实的资源（数据库）要注入。
type server struct {
	db *sql.DB
}

// openDB 以只读姿态打开历史库。query_only 是兜底：SQLite 层面拒绝一切写操作。
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4) // 只读查询可以并发，不像写入需要单连接串行化
	for _, p := range []string{
		"PRAGMA busy_timeout=5000", // 与写入方撞锁时等 5s
		"PRAGMA query_only=ON",     // 物理禁写：模型乱调参数也伤不到数据
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return db, nil
}

// toolDef 工具定义（同 toolsdemo：描述 + JSON Schema + handler）。
type toolDef struct {
	description string
	inputSchema map[string]any
	handler     func(s *server, args json.RawMessage) (string, error)
}

var registry = map[string]toolDef{}

func init() {
	registry["list_sessions"] = toolDef{
		description: "列出最近的面试场次，返回每场的时间、岗位、综合得分。limit 默认 10，上限 50",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "返回条数，1~50，默认 10"},
			},
		},
		handler: handleListSessions,
	}
	registry["get_report"] = toolDef{
		description: "按场次 ID 取回完整 Markdown 评分报告（系列一第七篇落库的那份）",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "场次 ID，来自 list_sessions"},
			},
			"required": []string{"id"},
		},
		handler: handleGetReport,
	}
	registry["get_session"] = toolDef{
		description: "按场次 ID 查看该场每道题的维度、置信度、要点覆盖度和追问次数",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "场次 ID，来自 list_sessions"},
			},
			"required": []string{"id"},
		},
		handler: handleGetSession,
	}
}

// handleListSessions 全库按时间倒序列场次。 limit <= 0 用默认值，超过上限截断——
// 对模型友好：参数差一点也能工作，而不是直接报错让模型重试。
func handleListSessions(s *server, args json.RawMessage) (string, error) {
	var in struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("arguments 不是合法 JSON: %w", err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	rows, err := s.db.Query(
		`SELECT id, chat_key, position, overall, created_at
		 FROM interviews ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var id, overall int
		var key, position, created string
		if err := rows.Scan(&id, &key, &position, &overall, &created); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("#%d | %s | %s | 综合 %d 分 | 会话 %s",
			id, created, position, overall, key))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("历史库为空：先用面试教练跑一场面试（系列一）")
	}
	return "最近面试场次（新→旧）：\n" + strings.Join(lines, "\n"), nil
}

// handleGetReport 按 ID 取完整报告。ErrNoRows 是业务错误（isError），
// 让模型自己意识到「ID 猜错了，应该先 list_sessions」。
func handleGetReport(s *server, args json.RawMessage) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("arguments 不是合法 JSON: %w", err)
	}
	if in.ID <= 0 {
		return "", fmt.Errorf("id 必须是正整数")
	}
	var rep string
	err := s.db.QueryRow(`SELECT report_md FROM interviews WHERE id = ?`, in.ID).Scan(&rep)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("场次 #%d 不存在，可先调用 list_sessions 查看可用 ID", in.ID)
	}
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}
	return rep, nil
}

// handleGetSession 按 ID 看该场逐题指标——比报告更适合「我哪类题总被追问」这类对比分析。
func handleGetSession(s *server, args json.RawMessage) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("arguments 不是合法 JSON: %w", err)
	}
	if in.ID <= 0 {
		return "", fmt.Errorf("id 必须是正整数")
	}
	var position string
	var overall int
	var created string
	err := s.db.QueryRow(
		`SELECT position, overall, created_at FROM interviews WHERE id = ?`, in.ID).
		Scan(&position, &overall, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("场次 #%d 不存在，可先调用 list_sessions 查看可用 ID", in.ID)
	}
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT seq, dimension, confidence, coverage, followups, question
		 FROM answers WHERE interview_id = ? ORDER BY seq`, in.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	header := fmt.Sprintf("#%d | %s | %s | 综合 %d 分", in.ID, created, position, overall)
	var lines []string
	for rows.Next() {
		var seq, confidence, followups int
		var dimension, question string
		var coverage float64
		if err := rows.Scan(&seq, &dimension, &confidence, &coverage, &followups, &question); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("第%d题 [%s] 置信度%d 覆盖度%.0f%% 追问%d次 | %s",
			seq, dimension, confidence, coverage*100, followups, question))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return header + "\n" + strings.Join(lines, "\n"), nil
}

// handleRequest 分发器：与 toolsdemo 相同的骨架，但工具 handler 需要 server 状态。
func (s *server) handleRequest(req *rpcRequest) *rpcResponse {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // 通知：不回包
	}

	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "coachstore-mcp-server",
				"version": "0.3.0",
			},
		}

	case "ping":
		resp.Result = map[string]any{}

	case "tools/list":
		tools := make([]any, 0, len(registry))
		for name, def := range registry {
			tools = append(tools, map[string]any{
				"name":        name,
				"description": def.description,
				"inputSchema": def.inputSchema,
			})
		}
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
		text, err := def.handler(s, params.Arguments)
		if err != nil {
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

	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func main() {
	path := os.Getenv("COACH_DB_PATH")
	if path == "" {
		path = "./coach.db" // 与 cmd/coach 的默认约定一致
	}

	db, err := openDB(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开历史库失败:", err)
		os.Exit(1)
	}
	defer db.Close()

	s := &server{db: db}
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

		resp := s.handleRequest(&req)
		if resp == nil {
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
