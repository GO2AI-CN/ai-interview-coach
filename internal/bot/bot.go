// Package bot 把面试教练接入 IM（飞书长连接）。
//
// 设计核心：IM 只是一层新的 I/O 适配器——
// 会话路由（谁正在面试）、指令解析、回复拼装都在本包，
// 出题 / 评估 / 追问 / 报告仍是 internal/coach 状态机，一行不改。
package bot

import (
	"context"
	"fmt"
	"io"
	"strconv"
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

// Transcriber 语音转文字（ASR）的最小抽象，生产实现见 internal/asr。
type Transcriber interface {
	Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error)
}

// AudioFetcher 按消息 ID + file_key 下载语音字节流，
// 返回 reader 和原始文件名（用于推断音频格式）。IM 层实现（飞书），核心不感知。
type AudioFetcher interface {
	Fetch(ctx context.Context, messageID, fileKey string) (io.Reader, string, error)
}

// UsageMeter 可选：返回进程累计 LLM token 用量（prompt/completion 分列），
// 用于报告末尾附带本场消耗。传 nil 则不做统计，功能不受影响。
type UsageMeter func() (prompt, completion int64)

// Turn 一场面试里一题的完整记录（落库用）。
type Turn struct {
	Dimension  string  // 能力维度
	Question   string  // 题目（含追问改写后的最终文本）
	Answer     string  // 用户最终一次作答
	Confidence int     // 评估置信度 1-5
	Coverage   float64 // 要点覆盖度 0-1
	FollowUps  int     // 本题被追问了几次
}

// Recorder 可选：面试状态机实现此接口后，bot 才能把整场记录交给 Store。
// internal/coach 的适配器（lark.go 里的 coachInterview）实现了它；
// fake  Interview 不实现也不影响任何现有功能。
type Recorder interface {
	// Record 返回落库所需的整场数据。overall 由 Finish 内部顺带记录。
	Record() (position string, overall int, turns []Turn)
}

// HistoryItem 「历史」列表里的一行摘要。
type HistoryItem struct {
	ID        int64     // 落库主键，查看报告用
	Position  string    // 目标岗位
	Overall   int       // 综合得分 0-100
	CreatedAt time.Time // 本场结束时间
}

// Store 面试历史的持久化（可选注入，nil 则历史功能整体关闭）。
// 实现见 internal/store（SQLite）；接口定义在 bot 侧，
// 让存储实现依赖 bot 的抽象，而不是反过来。
type Store interface {
	// Save 一场完整面试：汇总 + 每题问答，返回落库 ID。
	Save(ctx context.Context, key, position string, overall int, reportMD string, turns []Turn) (int64, error)
	// Recent 该会话最近的 limit 场面试（新的在前）。
	Recent(ctx context.Context, key string, limit int) ([]HistoryItem, error)
	// Report 按 ID 取回完整 Markdown 报告；key 不匹配返回错误，防止跨会话串报告。
	Report(ctx context.Context, key string, id int64) (string, error)
}

// session 一个用户的一场进行中的面试。
// mu 把同一用户并发的多条消息串行化，避免把状态机打坏。
type session struct {
	it           Interview
	position     string
	expireAt     time.Time
	mu           sync.Mutex
	usedPrompt   int64 // 开考时累计用量快照，用于算本场差值
	usedComplete int64
}

// Bot IM 面试机器人。零值不可用，用 NewBot 构造。
type Bot struct {
	sender   Sender
	factory  Factory
	fetcher  AudioFetcher     // 可选：语音消息下载器（飞书资源接口）
	asr      Transcriber      // 可选：语音转文字；两者都配了才支持语音作答
	meter    UsageMeter       // 可选：LLM 用量统计，报告末尾附消耗
	store    Store            // 可选：面试历史落库；配了才有「历史」指令
	ttl      time.Duration    // 会话闲置多久后作废（默认 2 小时）
	sessions sync.Map         // key: chatID|senderOpenID -> *session
	history  sync.Map         // key -> []HistoryItem：最近一次「历史」列表，供「历史 N」选序号
	now      func() time.Time // 便于测试控制时间
}

// Option 给 NewBot 追加可选能力（语音识别等）。
type Option func(*Bot)

// WithAudio 注入语音能力：fetcher 下载语音字节流，tr 转文字。
// 只传一个或都不传时，语音作答会被优雅拒绝，不影响文字功能。
func WithAudio(fetcher AudioFetcher, tr Transcriber) Option {
	return func(b *Bot) {
		b.fetcher = fetcher
		b.asr = tr
	}
}

// WithUsageMeter 注入 LLM 用量统计；开了之后每场面试的报告末尾
// 会附本场 token 消耗（答得爽也要知道烧了多少）。
func WithUsageMeter(m UsageMeter) Option {
	return func(b *Bot) { b.meter = m }
}

// WithStore 注入面试历史存储；开了之后报告自动生成时落库，
// 用户可用「历史」回看最近几场、用「历史 N」调取第 N 场完整报告。
func WithStore(s Store) Option {
	return func(b *Bot) { b.store = s }
}

// NewBot 构造机器人。ttl<=0 时默认 2 小时。
func NewBot(sender Sender, factory Factory, ttl time.Duration, opts ...Option) *Bot {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	b := &Bot{sender: sender, factory: factory, ttl: ttl, now: time.Now}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

const helpText = `我是 AI 模拟面试教练 🤖 指令如下：
  面试 <目标岗位>   开一场新面试（如：面试 Go 后端工程师）
  结束              提前结束并出评分报告
  历史              查看最近几场面试（含得分）
  历史 <序号>       查看某一场的完整评分报告
  帮助              显示本说明
开考后文字、语音作答都可以；我会先语音转文字（回复里会附上识别结果），
再逐题评估，答得浅会自动追问。`

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

	// 指令：历史（列表 / 按序号取报告）
	if reply, ok := b.handleHistory(ctx, key, text); ok {
		return reply, nil
	}

	// 指令：开新面试
	if pos, ok := strings.CutPrefix(text, "面试 "); ok && strings.TrimSpace(pos) != "" {
		it, err := b.factory(ctx, strings.TrimSpace(pos))
		if err != nil {
			return "", fmt.Errorf("生成面试计划失败: %w", err)
		}
		s := &session{it: it, position: strings.TrimSpace(pos), expireAt: b.now().Add(b.ttl)}
		if b.meter != nil {
			s.usedPrompt, s.usedComplete = b.meter() // 用量快照：本场差值的起点
		}
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
		return "面试已提前结束，评分报告：\n\n" + rep + b.usageLine(s) + b.saveHistory(ctx, key, s, rep), nil
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
	return feedback + "\n\n面试结束 🎉 评分报告：\n\n" + rep + b.usageLine(s) + b.saveHistory(ctx, key, s, rep), nil
}

// handleHistory 处理「历史」和「历史 N」。第二个返回值表示是否命中指令。
// IM 列表交互的经典做法：先给带序号的列表并缓存，用户回复序号即可定位。
func (b *Bot) handleHistory(ctx context.Context, key, text string) (string, bool) {
	if text != "历史" {
		nStr, ok := strings.CutPrefix(text, "历史 ")
		if !ok {
			return "", false
		}
		if b.store == nil {
			return "历史功能未开启。", true
		}
		return b.historyReport(ctx, key, strings.TrimSpace(nStr)), true
	}
	if b.store == nil {
		return "历史功能未开启。", true
	}
	items, err := b.store.Recent(ctx, key, 5)
	if err != nil {
		return "查询历史失败：" + err.Error(), true
	}
	if len(items) == 0 {
		return "还没有面试记录，输入「面试 <目标岗位>」开始第一场～", true
	}
	b.history.Store(key, items)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 最近 %d 场面试：\n", len(items)))
	for i, it := range items {
		fmt.Fprintf(&sb, "%d. %s · 综合 %d 分 · %s\n",
			i+1, it.Position, it.Overall, it.CreatedAt.Format("01-02 15:04"))
	}
	sb.WriteString("\n回复「历史 序号」查看对应完整报告。")
	return sb.String(), true
}

// historyReport 按序号取报告；序号来自最近一次「历史」列表的缓存。
func (b *Bot) historyReport(ctx context.Context, key, nStr string) string {
	v, ok := b.history.Load(key)
	if !ok {
		return "请先回复「历史」获取列表，再回复「历史 序号」选择。"
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 1 || n > len(v.([]HistoryItem)) {
		return "序号无效，请先回复「历史」获取最新列表。"
	}
	item := v.([]HistoryItem)[n-1]
	rep, err := b.store.Report(ctx, key, item.ID)
	if err != nil {
		return "查询报告失败：" + err.Error() // 报告可能已被清理，按提示处理即可
	}
	return fmt.Sprintf("📄 %s · 综合 %d 分 · %s\n\n%s",
		item.Position, item.Overall, item.CreatedAt.Format("01-02 15:04"), rep)
}

// saveHistory 报告落库。失败不阻塞回复（报告正文已经发出），只附一行警告。
// 状态机实现了 Recorder 才落库；返回追加在报告末尾的说明文字。
func (b *Bot) saveHistory(ctx context.Context, key string, s *session, reportMD string) string {
	if b.store == nil {
		return ""
	}
	rec, ok := s.it.(Recorder)
	if !ok {
		return ""
	}
	pos, overall, turns := rec.Record()
	if _, err := b.store.Save(ctx, key, pos, overall, reportMD, turns); err != nil {
		return "\n\n（⚠️ 本场历史落库失败：" + err.Error() + "）"
	}
	return "\n\n本场记录已存入历史，回复「历史」随时回看。"
}

// usageLine 报告末尾的本场用量说明；未注入统计时返回空串。
func (b *Bot) usageLine(s *session) string {
	if b.meter == nil {
		return ""
	}
	p, c := b.meter()
	return fmt.Sprintf("\n\n———\n本场 LLM 消耗：%d tokens（输入 %d / 输出 %d）",
		p+c-s.usedPrompt-s.usedComplete, p-s.usedPrompt, c-s.usedComplete)
}

// HandleAudio 处理一条语音作答：先 ASR 转文字，再走与文字消息完全
// 相同的指令/作答管线——所以对着机器人说「结束」也能正常出报告。
// audio 是语音字节流，filename 仅用于推断格式，可为空。
func (b *Bot) HandleAudio(ctx context.Context, key string, audio io.Reader, filename string) (string, error) {
	if b.asr == nil {
		return "这个机器人没开语音识别，请用文字作答～", nil
	}
	text, err := b.asr.Transcribe(ctx, audio, filename)
	if err != nil {
		return "", fmt.Errorf("语音识别失败: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "这段语音没听清，请靠近话筒、说普通话再试一次～", nil
	}
	reply, err := b.HandleText(ctx, key, text)
	if err != nil {
		return "", err
	}
	if reply == "" {
		return "", nil
	}
	// 把识别结果前置展示：评估以它为准，用户能当场发现识别偏差
	return "【语音识别】" + text + "\n\n" + reply, nil
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
