// Package cost 实现《LLM 应用工程化》系列第 4 篇的成本计量与限流。
// 用量数据复用项目进程级计数器（internal/llm.Usage）的差值。
package cost

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ai-interview-coach/internal/llm"
)

// Price 单模型单价（美元 / 1M tokens）
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// CostUSD 计算一次用量的成本
func CostUSD(u llm.Usage, p Price) float64 {
	return float64(u.PromptTokens)/1e6*p.Input + float64(u.CompletionTokens)/1e6*p.Output
}

// Meter 成本计量器：价格表 + 按日聚合 + 当日预算熔断
type Meter struct {
	mu     sync.Mutex
	prices map[string]Price
	day    string
	acc    map[string]float64 // model -> 当日累计成本（美元）
	limit  float64            // 当日预算（美元），0 表示不限
}

func NewMeter(prices map[string]Price, dailyLimitUSD float64) *Meter {
	return &Meter{
		prices: prices,
		day:    time.Now().Format("2006-01-02"),
		acc:    map[string]float64{},
		limit:  dailyLimitUSD,
	}
}

// Allow 调用前检查：当日该模型预算是否未耗尽
func (m *Meter) Allow(model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.limit <= 0 {
		return true
	}
	return m.acc[model] < m.limit
}

// Record 调用后记账（用量来自服务端 usage 累计差值），输出结构化日志
func (m *Meter) Record(model string, u llm.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != m.day { // 跨天自动清零
		m.day, m.acc = today, map[string]float64{}
	}
	p := m.PriceOf(model)
	c := CostUSD(u, p)
	m.acc[model] += c
	slog.Info("llm_cost",
		"model", model,
		"prompt_tokens", u.PromptTokens,
		"completion_tokens", u.CompletionTokens,
		"cost_usd", fmt.Sprintf("%.6f", c),
		"day_total_usd", fmt.Sprintf("%.4f", m.acc[model]),
	)
}

// PriceOf 返回某模型的单价（未配置返回保守默认值）
func (m *Meter) PriceOf(model string) Price {
	if p, ok := m.prices[model]; ok {
		return p
	}
	return Price{Input: 2.5, Output: 10}
}

// DayTotal 当日总成本（可暴露给 /metrics）
func (m *Meter) DayTotal() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t float64
	for _, v := range m.acc {
		t += v
	}
	return t
}
