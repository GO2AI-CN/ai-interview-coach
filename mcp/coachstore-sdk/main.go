// mcp/coachstore-sdk —— 用官方 SDK 重写 mcp/coachstore（系列二第 8 篇第二节的实战兑现）。
//
// 与手写版对照的三个关键差异：
//  1. 工具 schema 不再手写 JSON——AddTool 从输入结构体的类型 + jsonschema tag
//     自动推导，「改结构体即改协议」；
//  2. handler 直接收反序列化好的 typed input，arguments 解析与 schema 校验
//     SDK 在进 handler 之前做完；
//  3. handler 返回的 error 自动打包成 isError 工具结果（协议约定：
//     业务错误引路给模型，而非 JSON-RPC 报错）——手写版那段手工拼 map 没了。
//
// 数据面与手写版完全一致：database/sql + query_only=ON 物理禁写 + busy_timeout。
// 两个 server 可互换使用（client 任选一个连）。
//
// 用法：COACH_DB_PATH=./coach.db go run ./mcp/coachstore-sdk
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动，纯 Go 无 CGO
)

// defaultLimit / maxLimit 约束 list_sessions 的返回条数，
// 工具结果会整个塞进模型上下文，无上限是自杀行为。
const (
	defaultLimit = 10
	maxLimit     = 50
)

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

// listSessionsIn / byIDIn 是工具入参类型：jsonschema tag 即文档，
// AddTool 据此自动生成 inputSchema 并做入参校验（required 字段缺失在
// 进 handler 前就被拒，手写版那些 "arguments 不是合法 JSON" 分支全省了）。
type listSessionsIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"返回条数，1~50，默认 10"`
}
type byIDIn struct {
	ID int64 `json:"id" jsonschema:"required,场次 ID，来自 list_sessions"`
}

// textResult 把手写版里到处重复的「拼 content map」收成一行。
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// listSessions 全库按时间倒序列场次。limit <= 0 用默认值，超过上限截断——
// 对模型友好：参数差一点也能工作，而不是直接报错让模型重试。
func listSessions(db *sql.DB) mcp.ToolHandlerFor[listSessionsIn, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in listSessionsIn) (
		*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = defaultLimit
		}
		if limit > maxLimit {
			limit = maxLimit
		}

		rows, err := db.Query(
			`SELECT id, chat_key, position, overall, created_at
			 FROM interviews ORDER BY id DESC LIMIT ?`, limit)
		if err != nil {
			return nil, nil, fmt.Errorf("查询失败: %w", err)
		}
		defer rows.Close()

		var lines []string
		for rows.Next() {
			var id, overall int
			var key, position, created string
			if err := rows.Scan(&id, &key, &position, &overall, &created); err != nil {
				return nil, nil, err
			}
			lines = append(lines, fmt.Sprintf("#%d | %s | %s | 综合 %d 分 | 会话 %s",
				id, created, position, overall, key))
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		if len(lines) == 0 {
			// 返回 error = 业务错误：SDK 自动打包成 isError 结果引路给模型
			return nil, nil, errors.New("历史库为空：先用面试教练跑一场面试（系列一）")
		}
		return textResult("最近面试场次（新→旧）：\n" + strings.Join(lines, "\n")), nil, nil
	}
}

// getReport 按 ID 取完整报告。ErrNoRows 是业务错误（isError），
// 让模型自己意识到「ID 猜错了，应该先 list_sessions」。
func getReport(db *sql.DB) mcp.ToolHandlerFor[byIDIn, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in byIDIn) (
		*mcp.CallToolResult, any, error) {
		if in.ID <= 0 {
			return nil, nil, errors.New("id 必须是正整数")
		}
		var rep string
		err := db.QueryRow(`SELECT report_md FROM interviews WHERE id = ?`, in.ID).Scan(&rep)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("场次 #%d 不存在，可先调用 list_sessions 查看可用 ID", in.ID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("查询失败: %w", err)
		}
		return textResult(rep), nil, nil
	}
}

// getSession 按 ID 看该场逐题指标——比报告更适合「我哪类题总被追问」这类对比分析。
func getSession(db *sql.DB) mcp.ToolHandlerFor[byIDIn, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in byIDIn) (
		*mcp.CallToolResult, any, error) {
		if in.ID <= 0 {
			return nil, nil, errors.New("id 必须是正整数")
		}
		var position string
		var overall int
		var created string
		err := db.QueryRow(
			`SELECT position, overall, created_at FROM interviews WHERE id = ?`, in.ID).
			Scan(&position, &overall, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("场次 #%d 不存在，可先调用 list_sessions 查看可用 ID", in.ID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("查询失败: %w", err)
		}

		rows, err := db.Query(
			`SELECT seq, dimension, confidence, coverage, followups, question
			 FROM answers WHERE interview_id = ? ORDER BY seq`, in.ID)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()

		header := fmt.Sprintf("#%d | %s | %s | 综合 %d 分", in.ID, created, position, overall)
		var lines []string
		for rows.Next() {
			var seq, confidence, followups int
			var dimension, question string
			var coverage float64
			if err := rows.Scan(&seq, &dimension, &confidence, &coverage, &followups, &question); err != nil {
				return nil, nil, err
			}
			lines = append(lines, fmt.Sprintf("第%d题 [%s] 置信度%d 覆盖度%.0f%% 追问%d次 | %s",
				seq, dimension, confidence, coverage*100, followups, question))
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		return textResult(header + "\n" + strings.Join(lines, "\n")), nil, nil
	}
}

// buildServer 组装三个工具。对照手写版的 init() 注册表：
// 每个工具从「手写 inputSchema JSON + RawMessage 解析」变成
// 「一个结构体 + 一个闭包」，注册表本身由 SDK 管理。
func buildServer(db *sql.DB) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "coachstore", Version: "0.3.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "列出最近的面试场次，返回每场的时间、岗位、综合得分。limit 默认 10，上限 50",
	}, listSessions(db))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_report",
		Description: "按场次 ID 取回完整 Markdown 评分报告（系列一第七篇落库的那份）",
	}, getReport(db))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_session",
		Description: "按场次 ID 查看该场每道题的维度、置信度、要点覆盖度和追问次数",
	}, getSession(db))
	return s
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

	// 阻塞运行；client 断开（stdin EOF）才返回——手写版那个
	// scanner 循环 + 请求分发器（约 150 行）全是 SDK 的活了。
	if err := buildServer(db).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "server 异常退出:", err)
		os.Exit(1)
	}
}
