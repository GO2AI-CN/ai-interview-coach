// Package store 把面试历史落库到 SQLite（modernc.org/sqlite 纯 Go 实现，
// 无需 CGO，交叉编译和部署零成本）。
//
// 依赖方向：本包 import bot 包来实现 bot.Store 接口——
// 抽象（接口）定义在消费侧 bot，存储实现单向依赖它，
// bot 不 import store，传 nil 即整体关闭历史功能。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动，纯 Go 无 CGO

	"ai-interview-coach/internal/bot"
)

// SQLite 基于本地 SQLite 文件的 bot.Store 实现。
type SQLite struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库文件并建表。path 例："./coach.db"。
//
// 并发策略：SQLite 同一时刻只允许一个写入者。与其等并发写互撞
// busy 错误再重试，不如在连接池这一层直接串行化——MaxOpenConns(1)
// 让所有读写排队走单连接，配合 busy_timeout 兜底，代码里最省心。
// （bot 侧每会话虽有 mutex，但 store 是进程级共享资源，防的是会话之间。）
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",  // WAL：读写不互堵，崩溃可恢复
		"PRAGMA busy_timeout=5000", // 撞锁等 5s，而不是立刻 SQLITE_BUSY
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	return &SQLite{db: db}, nil
}

// schema 两张表起步：interviews 存每场汇总（报告 Markdown 整段存，
// 不拆维度表——报告是给人读的文档，不是要做聚合分析的数据）。
const schema = `
CREATE TABLE IF NOT EXISTS interviews (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_key   TEXT NOT NULL,             -- 会话标识：一场面试属于一个 IM 会话
    position   TEXT NOT NULL,             -- 目标岗位
    overall    INTEGER NOT NULL,          -- 综合得分 0-100
    report_md  TEXT NOT NULL,             -- 完整 Markdown 评分报告
    created_at TEXT NOT NULL              -- RFC3339，本场结束时间
);
CREATE TABLE IF NOT EXISTS answers (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    interview_id INTEGER NOT NULL REFERENCES interviews(id),
    seq          INTEGER NOT NULL,        -- 题号，从 1 开始
    dimension    TEXT NOT NULL,           -- 能力维度
    question     TEXT NOT NULL,           -- 题目（含追问改写后的最终文本）
    answer       TEXT NOT NULL,           -- 最终作答
    confidence   INTEGER NOT NULL,        -- 评估置信度 1-5
    coverage     REAL NOT NULL,           -- 要点覆盖度 0-1
    followups    INTEGER NOT NULL DEFAULT 0 -- 本题追问次数
);
CREATE INDEX IF NOT EXISTS idx_interviews_key_time ON interviews(chat_key, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_answers_interview ON answers(interview_id);
`

// Close 关闭数据库。
func (s *SQLite) Close() error { return s.db.Close() }

// Save 一场完整面试：汇总 + 每题问答，单事务写入，要么全成要么全不成。
func (s *SQLite) Save(ctx context.Context, key, position string, overall int, reportMD string, turns []bot.Turn) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // 成功后 Commit 会把它变成 no-op

	res, err := tx.ExecContext(ctx,
		`INSERT INTO interviews (chat_key, position, overall, report_md, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, position, overall, reportMD, time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("写 interviews 失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, t := range turns {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO answers (interview_id, seq, dimension, question, answer, confidence, coverage, followups)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, i+1, t.Dimension, t.Question, t.Answer, t.Confidence, t.Coverage, t.FollowUps)
		if err != nil {
			return 0, fmt.Errorf("写 answers 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交事务失败: %w", err)
	}
	return id, nil
}

// Recent 该会话最近的 limit 场面试，新的在前。
func (s *SQLite) Recent(ctx context.Context, key string, limit int) ([]bot.HistoryItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, position, overall, created_at FROM interviews
		 WHERE chat_key = ? ORDER BY id DESC LIMIT ?`, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []bot.HistoryItem
	for rows.Next() {
		var it bot.HistoryItem
		var created string
		if err := rows.Scan(&it.ID, &it.Position, &it.Overall, &created); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, created)
		if err != nil {
			return nil, fmt.Errorf("解析 created_at %q: %w", created, err)
		}
		it.CreatedAt = t
		items = append(items, it)
	}
	return items, rows.Err()
}

// Report 按 ID 取回完整 Markdown 报告；chat_key 不匹配返回错误，
// 防止一个会话通过猜 ID 看到别人的报告。
func (s *SQLite) Report(ctx context.Context, key string, id int64) (string, error) {
	var rep string
	err := s.db.QueryRowContext(ctx,
		`SELECT report_md FROM interviews WHERE id = ? AND chat_key = ?`, id, key).Scan(&rep)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("报告不存在或不属于当前会话")
	}
	if err != nil {
		return "", err
	}
	return rep, nil
}
