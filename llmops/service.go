// Package llmops 是《LLM 应用工程化》系列（8 篇）的总装配：
// 把 Registry、结构化防线、评测集、成本计量、语义缓存、Trace、安全护栏
// 组装成业务侧唯一入口 Service。
package llmops

import (
	"context"
	"embed"
	"errors"
	"time"

	"ai-interview-coach/internal/llm"
	"ai-interview-coach/llmops/cache"
	"ai-interview-coach/llmops/cost"
	"ai-interview-coach/llmops/guard"
	"ai-interview-coach/llmops/obs"
	"ai-interview-coach/llmops/registry"
)

// promptsFS 内嵌 Prompt 资产（embed 不能引用上级目录，prompts 放在本包下）
//
//go:embed prompts
var promptsFS embed.FS

// Embeder 问题文本 -> 向量；生产实现调 Embedding API，可为 nil（关闭缓存）
type Embeder func(ctx context.Context, text string) ([]float32, error)

// Config Service 装配配置
type Config struct {
	Model      string
	PromptName string
	PromptVer  string // "latest" 或钉死版本（如 "v2"）
	CacheTTL   time.Duration
	Prices     map[string]cost.Price
	DailyLimit float64 // 美元，0 不限
}

// Service 业务侧唯一入口：护栏 → 缓存 → 成本 → LLM → 防线 → 消毒 → 回写
type Service struct {
	Reg       *registry.Registry
	Cache     *cache.SemanticCache
	Meter     *cost.Meter
	Detector  *guard.InjectionDetector
	Embeder   Embeder
	LLM       llm.Client
	validator *validatorSchema

	Model, PromptName, PromptVer string
	CacheTTL                     time.Duration
	Vars                         map[string]string // Prompt 模板变量
}

// New 从嵌入的 prompts 目录装配 Service
func New(cfg Config, client llm.Client, embeder Embeder) (*Service, error) {
	reg, err := registry.Load(promptsFS, "prompts")
	if err != nil {
		return nil, err
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 24 * time.Hour
	}
	validator, err := compileValidator()
	if err != nil {
		return nil, err
	}
	return &Service{
		Reg:        reg,
		Cache:      cache.NewSemanticCache(0.90),
		Meter:      cost.NewMeter(cfg.Prices, cfg.DailyLimit),
		Detector:   guard.NewInjectionDetector(),
		Embeder:    embeder,
		LLM:        client,
		validator:  validator,
		Model:      cfg.Model,
		PromptName: cfg.PromptName,
		PromptVer:  cfg.PromptVer,
		CacheTTL:   cfg.CacheTTL,
		Vars:       map[string]string{"product": "Go2AI 知识社区", "tone": "亲切专业"},
	}, nil
}

// ErrBudgetExhausted 当日预算耗尽
var ErrBudgetExhausted = errors.New("daily budget exhausted")

// ErrInjectionDetected 输入命中注入特征
var ErrInjectionDetected = errors.New("输入包含可疑指令内容")

// Analyze 一次完整业务调用（对应系列第 8 篇「运行视图」）
func (s *Service) Analyze(ctx context.Context, requestID, ticket string) (*guard.Review, error) {
	ctx, span := obs.StartSpan(ctx, "llm.pipeline", obs.SpanMeta{
		Model: s.Model, PromptName: s.PromptName, PromptVer: s.PromptVer,
	})
	defer span.End()

	// ⑦ 安全护栏
	if r := s.Detector.Scan(ticket); r.Hit {
		obs.RecordInjection(ctx, r.Matches)
		return nil, ErrInjectionDetected
	}

	// ① Prompt 解析 + 决策日志
	prompt, err := registry.PickVersion(s.Reg, s.PromptName, s.PromptVer, 0.2, requestID)
	if err != nil {
		return nil, err
	}
	latest, _ := s.Reg.Get(s.PromptName, "latest")
	registry.LogDecision(&registry.DecisionLog{
		Time: time.Now(), RequestID: requestID,
		PromptName: s.PromptName, Version: prompt.Version,
		RolloutHit: prompt.Version == latest.Version,
	})
	systemPrompt, err := prompt.Render(s.Vars)
	if err != nil {
		return nil, err
	}

	// ⑤ 语义缓存（需要 Embeder）
	if s.Embeder != nil {
		emb, err := s.Embeder(ctx, ticket)
		if err != nil {
			return nil, err
		}
		if e, sim, hit := s.Cache.Get(cache.Key{Embedding: emb, Model: s.Model, PromptVer: prompt.Version}); hit {
			obs.RecordCacheHit(ctx, sim)
			return guard.DecodeReview(e.Answer)
		}
	}

	// ④ 预算熔断
	if !s.Meter.Allow(s.Model) {
		return nil, ErrBudgetExhausted
	}

	// ② 结构化防线（含修复循环）
	review, usage, err := guard.Extract(ctx, s.LLM, s.validator, systemPrompt, ticket)
	if err != nil {
		return nil, err
	}
	s.Meter.Record(s.Model, usage)
	obs.RecordUsage(ctx, usage.PromptTokens, usage.CompletionTokens,
		cost.CostUSD(usage, s.Meter.PriceOf(s.Model)), "stop")

	// ⑦ 输出消毒（对 summary 中可能出现的外链）
	review.Summary = guard.SanitizeOutput(review.Summary)

	// ⑤ 回写缓存
	if s.Embeder != nil {
		emb, _ := s.Embeder(ctx, ticket)
		s.Cache.Set(cache.Entry{
			Key:       cache.Key{Embedding: emb, Model: s.Model, PromptVer: prompt.Version},
			Question:  ticket,
			Answer:    guard.EncodeReview(review),
			CreatedAt: time.Now(),
			TTL:       s.CacheTTL,
		})
	}
	return review, nil
}
