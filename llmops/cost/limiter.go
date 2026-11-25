package cost

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// UserLimiter 第 1 层限流：每用户令牌桶。注意 map 只增不减，
// 生产需加闲置清扫；演示与中小规模直接使用
type UserLimiter struct {
	mu      sync.Mutex
	lim     map[string]*rate.Limiter
	perUser rate.Limit
	burst   int
}

func NewUserLimiter(qps rate.Limit, burst int) *UserLimiter {
	return &UserLimiter{lim: map[string]*rate.Limiter{}, perUser: qps, burst: burst}
}

func (l *UserLimiter) Allow(userID string) bool {
	l.mu.Lock()
	lim, ok := l.lim[userID]
	if !ok {
		lim = rate.NewLimiter(l.perUser, l.burst)
		l.lim[userID] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}

// ConcurrencyGate 第 2 层限流：全局并发闸门（buffered channel 实现），
// 排队等待带超时，超时快速失败返回 429
type ConcurrencyGate chan struct{}

func NewConcurrencyGate(max int) ConcurrencyGate { return make(chan struct{}, max) }

func (g ConcurrencyGate) Acquire(ctx context.Context) error {
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (g ConcurrencyGate) Release() { <-g }
