// Package bot 把面试教练接入 IM（飞书长连接）。
//
// 设计核心：IM 只是一层新的 I/O 适配器——
// 会话路由（谁正在面试）、指令解析、回复拼装都在本包，
// 出题 / 评估 / 追问 / 报告仍是 internal/coach 状态机，一行不改。
package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Sender 把回复文本发回 IM。真实实现走飞书 OpenAPI，测试时替换为 fake。
type Sender interface {
	Reply(ctx context.Context, messageID, text string) error
}

// Interview 是 bot 对面试状态机的最小依赖面，便于用 fake 单测。
// 实现见 lark.go 里的 coachInterview（包装 internal/coach.Interview）。
type Interview interface {
	// Question 返回当前问题文本（含维度标签）；全部答完返回 ""。
	Question() string
	// Answer 提交一题作答。followUp=true 表示触发了追问；
	// 返回的 feedback 永远是「评估摘要 + 下一步」的完整回复文本。
	Answer(ctx context.Context, answer string) (followUp bool, feedback string, err error)
	// Finish 提前或正常结束面试，返回 Markdown 评分报告。
	Finish(ctx context.Context) (string, error)
}

// Factory 按目标岗位开一场新面试。
type Factory func(ctx context.Context, position string) (Interview, error)

// session 一个用户的一场进行中的面试。
// mu 把同一用户并发的多条消息串行化，避免把状态机打坏。
type session struct {
	it       Interview
	position string
	expireAt time.Time
	mu       sync.Mutex
}

// Bot IM 面试机器人。零值不可用，用 NewBot 构造。
type Bot struct {
	sender   Sender
	factory  Factory
	ttl      time.Duration    // 会话闲置多久后作废（默认 2 小时）
	sessions sync.Map         // key: chatID|senderOpenID -> *session
	now      func() time.Time // 便于测试控制时间
}

// NewBot 构造机器人。ttl<=0 时默认 2 小时。
func NewBot(sender Sender, factory Factory, ttl time.Duration) *Bot {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &Bot{sender: sender, factory: factory, ttl: ttl, now: time.Now}
}

const helpText = `我是 AI 模拟面试教练 🤖 指令如下：
  面试 <目标岗位>   开一场新面试（如：面试 Go 后端工程师）
  结束              提前结束并出评分报告
  帮助              显示本说明
开考后直接文字作答即可，我会逐题评估、答得浅会自动追问。`

// HandleText 处理一条已经提取好的纯文本消息。
// key 是会话标识（通常 chatID|senderOpenID），text 是去掉 @ 机器人后的文本。
// 返回要回复的文本；空串表示无需回复。error 只代表系统级故障。
func (b *Bot) HandleText(ctx context.Context, key, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}

	// 指令：帮助
	if text == "帮助" || text == "help" {
		return helpText, nil
	}

	// 指令：开新面试
	if pos, ok := strings.CutPrefix(text, "面试 "); ok && strings.TrimSpace(pos) != "" {
		it, err := b.factory(ctx, strings.TrimSpace(pos))
		if err != nil {
			return "", fmt.Errorf("生成面试计划失败: %w", err)
		}
		s := &session{it: it, position: strings.TrimSpace(pos), expireAt: b.now().Add(b.ttl)}
		b.sessions.Store(key, s)
		return "面试计划已生成：" + it.Question() + "\n\n直接回复你的答案即可。", nil
	}

	// 指令：提前结束
	if text == "结束" {
		s := b.load(key)
		if s == nil {
			return "你还没有进行中的面试，输入「面试 <岗位>」开始。", nil
		}
		b.sessions.Delete(key)
		rep, err := s.it.Finish(ctx)
		if err != nil {
			return "", err
		}
		return "面试已提前结束，评分报告：\n\n" + rep, nil
	}

	// 非指令：必须是进行中的作答
	s := b.load(key)
	if s == nil {
		return "输入「面试 <目标岗位>」开始一场模拟面试，或输入「帮助」查看说明。", nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireAt = b.now().Add(b.ttl) // 每次作答重置闲置时钟

	followUp, feedback, err := s.it.Answer(ctx, text)
	if err != nil {
		return "", err
	}
	if followUp {
		return feedback, nil // feedback 里已带追问
	}
	if next := s.it.Question(); next != "" {
		return feedback + "\n\n" + next, nil
	}
	// 最后一题答完：出报告，回收会话
	b.sessions.Delete(key)
	rep, err := s.it.Finish(ctx)
	if err != nil {
		return "", err
	}
	return feedback + "\n\n面试结束 🎉 评分报告：\n\n" + rep, nil
}

// load 取会话并做惰性过期检查。
func (b *Bot) load(key string) *session {
	v, ok := b.sessions.Load(key)
	if !ok {
		return nil
	}
	s := v.(*session)
	if b.now().After(s.expireAt) {
		b.sessions.Delete(key)
		return nil
	}
	return s
}
