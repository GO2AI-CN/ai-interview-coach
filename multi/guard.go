package multi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCircuitOpen 表示熔断器打开，调用被快速拒绝、没有触达下游 Agent。
var ErrCircuitOpen = errors.New("multi: 熔断中，调用被拒绝")

// Guard 给单个 Agent 装四道保险丝：超时、重试、熔断、降级。
// Guard 本身实现 Agent 接口，可以直接 Register 进 Orchestrator。
type Guard struct {
	agent Agent

	timeout     time.Duration                  // 单次调用最长耗时
	maxAttempts int                            // 总尝试次数（含首次）
	breakAfter  int                            // 连续失败多少次后熔断
	cooldown    time.Duration                  // 熔断冷却时长，过后放一次试探调用
	fallback    func(t Task) (Result, error)   // 降级：彻底失败时的兜底产出

	mu          sync.Mutex
	consecutive int       // 当前连续失败次数
	openUntil   time.Time // 熔断打开的截止时间
}

// GuardOption 配置 Guard 的某一道保险丝。
type GuardOption func(*Guard)

// WithTimeout 设置单次调用超时。
func WithTimeout(d time.Duration) GuardOption {
	return func(g *Guard) { g.timeout = d }
}

// WithRetry 设置总尝试次数（含首次）。
func WithRetry(n int) GuardOption {
	return func(g *Guard) { g.maxAttempts = n }
}

// WithCircuitBreaker 设置熔断阈值与冷却时长。
func WithCircuitBreaker(after int, cooldown time.Duration) GuardOption {
	return func(g *Guard) { g.breakAfter, g.cooldown = after, cooldown }
}

// WithFallback 设置降级兜底：所有尝试都失败时产出备选结果。
func WithFallback(f func(t Task) (Result, error)) GuardOption {
	return func(g *Guard) { g.fallback = f }
}

// NewGuard 创建带默认参数的 Guard：5 秒超时、不重试、
// 连续 3 次失败熔断 2 秒、无降级兜底。
func NewGuard(a Agent, opts ...GuardOption) *Guard {
	g := &Guard{agent: a, timeout: 5 * time.Second, maxAttempts: 1, breakAfter: 3, cooldown: 2 * time.Second}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Name 透传被保护 Agent 的名字，编排者视角无感知。
func (g *Guard) Name() string { return g.agent.Name() }

// Run 执行被保护的调用：先试熔断，再按"超时+重试"循环，
// 全部失败则计数并走降级。
func (g *Guard) Run(ctx context.Context, t Task) (Result, error) {
	if err := g.tryAcquire(); err != nil {
		return g.degrade(t, err) // 熔断中：快速失败，不触达下游
	}
	var lastErr error
	for i := 0; i < g.maxAttempts; i++ {
		if i > 0 {
			time.Sleep(200 * time.Millisecond) // 重试间隔；生产可换指数退避
		}
		attemptCtx, cancel := context.WithTimeout(ctx, g.timeout)
		r, err := g.agent.Run(attemptCtx, t)
		cancel()
		if err == nil {
			g.onSuccess()
			return r, nil
		}
		lastErr = err
	}
	g.onFailure()
	return g.degrade(t, lastErr)
}

// tryAcquire 检查熔断状态：冷却期外放行（试探调用），冷却期内拒绝。
func (g *Guard) tryAcquire() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.openUntil) {
		return fmt.Errorf("%w（%s 后重试）", ErrCircuitOpen, g.openUntil.Sub(time.Now()).Round(time.Millisecond))
	}
	return nil
}

func (g *Guard) onSuccess() {
	g.mu.Lock()
	g.consecutive = 0 // 一次成功，计数清零，熔断解除
	g.mu.Unlock()
}

func (g *Guard) onFailure() {
	g.mu.Lock()
	g.consecutive++
	if g.consecutive >= g.breakAfter {
		g.openUntil = time.Now().Add(g.cooldown) // 熔断：冷却期内快速拒绝
	}
	g.mu.Unlock()
}

// degrade 走降级兜底；没配 fallback 就把错误原样抛给编排者。
func (g *Guard) degrade(t Task, cause error) (Result, error) {
	if g.fallback != nil {
		r, err := g.fallback(t)
		if err == nil {
			return r, nil // 降级成功：错误被兜住，对上游表现为正常
		}
	}
	return Result{}, cause
}
