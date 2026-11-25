# LLM 应用工程化④：Token 成本计量与限流，让账单从月底惊吓变成尽在掌握

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。前三篇解决了「稳」和「好」，本篇解决第三个维度：**钱**。LLM 应用有一种特有的财务事故：整个月没人管 Token，月底账单比服务器贵 20 倍。

成本事故的共同特征是「不可见」——直到账单出来那一刻，没人知道系统一个月烧了多少、谁烧的、烧在了哪个环节。所以成本治理的第一步从来不是省钱，而是**计量**：把每一次调用的成本变成一条结构化日志。计量有了，预算熔断和限流才有抓手。

## 一、计量的两个数据源：谁说了算

Token 数有两个来源，用途完全不同：

| 数据源 | 怎么拿 | 特点 | 用途 |
| --- | --- | --- | --- |
| **服务端 usage 字段** | API 响应里的 `usage.input_tokens / output_tokens` | 权威、零成本、调用后才知道 | 结算口径的成本核算 |
| **客户端预估**（tiktoken） | 本地分词计算 | 调用前可知、有极小误差 | 调用前的预算拦截、Prompt 长度保护 |

**结算用服务端数据，拦截用客户端预估**——这个分工记牢，后面代码里两者都会出现。

## 二、成本计量器：一次调用一条账

价格表必须做成配置而不是常量——厂商调价、多模型并存，写死的价格表三个月后一定说谎：

```go
package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
)

// Price 单模型单价（美元 / 1M tokens）
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// CostEntry 一次调用的成本记录
type CostEntry struct {
	Time        time.Time `json:"time"`
	Model       string    `json:"model"`
	InputTokens int64     `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CostUSD     float64   `json:"cost_usd"`
}

// Meter 成本计量器：价格表 + 按日聚合
type Meter struct {
	mu     sync.Mutex
	prices map[string]Price
	day    string            // YYYY-MM-DD
	acc    map[string]float64 // model -> 当日累计成本
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

// Allow 调用前检查：当日预算是否已耗尽（客户端拦截点）
func (m *Meter) Allow(model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.limit <= 0 {
		return true
	}
	return m.acc[model] < m.limit
}

// Record 调用后记账：以服务端 usage 为准
func (m *Meter) Record(model string, usage openai.ResponseUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != m.day { // 跨天自动清零
		m.day, m.acc = today, map[string]float64{}
	}
	p, ok := m.prices[model]
	if !ok {
		p = Price{Input: 2.5, Output: 10} // 未配置模型给个保守默认值
	}
	cost := float64(usage.InputTokens)/1e6*p.Input +
		float64(usage.OutputTokens)/1e6*p.Output
	m.acc[model] += cost

	slog.Info("llm_cost",
		"model", model,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cost_usd", fmt.Sprintf("%.6f", cost),
		"day_total_usd", fmt.Sprintf("%.4f", m.acc[model]),
	)
}

// DayTotal 当日总成本（暴露给 /metrics 或管理接口）
func (m *Meter) DayTotal() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t float64
	for _, v := range m.acc {
		t += v
	}
	return t
}
```

接线位置在调用层，三个钩子的顺序就是防线的层次：

```go
func (s *Service) Analyze(ctx context.Context, ticket string) (*Review, error) {
	model := "gpt-5.2"

	// 钩子 1：客户端预估拦截——输入超长直接拒，不浪费一次调用
	est := s.estimator.Count(ticket)
	if est > 8000 {
		return nil, fmt.Errorf("输入约 %d tokens，超出单请求预算 8000", est)
	}
	// 钩子 2：当日预算熔断
	if !s.meter.Allow(model) {
		slog.Warn("daily budget exhausted, degrade to cheap model", "model", model)
		model = "gpt-5-mini" // 降级到便宜模型，而不是直接失败——见第五节
	}

	resp, err := s.client.Responses.New(ctx, buildParams(model, ticket))
	if err != nil {
		return nil, err
	}
	// 钩子 3：调用后记账（服务端 usage 为准）
	s.meter.Record(model, resp.Usage)
	// ... 解析输出（第 2 篇的 Extract）
}
```

`resp.Usage` 就是服务端权威数据——Responses API 每次返回都带 `usage` 块，`openai.ResponseUsage` 里有 `InputTokens`、`OutputTokens`（SDK 字段名以 v3.107.0 实际导出为准，本文代码按官方 README 的 usage 访问方式核对）。

## 三、客户端预估：tiktoken 拦截超长输入

`tiktoken-go` 是 tiktoken 的 Go 移植，v0.1.8（2025-09 发布，截至本文发布日仍是最新版）：

```go
import "github.com/pkoukk/tiktoken-go"

type Estimator struct{ enc *tiktoken.Tiktoken }

func NewEstimator() *Estimator {
	enc, err := tiktoken.GetEncoding("o200k_base") // GPT-5 系列同款 BPE
	if err != nil {
		panic(err) // 启动期失败必须 panic，宁可起不来也不能裸奔
	}
	return &Estimator{enc: enc}
}

func (e *Estimator) Count(text string) int {
	return len(e.enc.Encode(text, nil, nil))
}
```

注意 `GetEncoding` 在启动期就要完成并 panic-on-fail：编码器加载失败意味着所有预估失效，继续启动等于把预算拦截整个拆掉。

## 四、限流：三层而不是一层

「防烧穿」靠单一层限流是不够的，实践中是三层各管一段：

```
第 1 层  用户级令牌桶    每用户每秒/每分钟的请求配额     → 防单用户刷爆
第 2 层  全局并发信号量  同时在飞的 LLM 请求数上限       → 防队列堆积拖垮服务
第 3 层  模型降级链      预算耗尽/限流时自动切便宜模型    → 保核心链路可用
```

第 1 层用 `golang.org/x/time/rate`（v0.16.0），一个 map 管理每用户桶：

```go
import "golang.org/x/time/rate"

type UserLimiter struct {
	mu      sync.Mutex
	lim     map[string]*rate.Limiter
	perUser rate.Limit // 例如 rate.Every(time.Second / 2) = 0.5 QPS
	burst   int        // 例如 5
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
```

map 只增不减是内存泄漏点——真实系统里给每个 limiter 加最后访问时间，定期清理闲置用户，或用 `sync.Map` + 后台清扫协程。

第 2 层用带权重的并发信号量，超时等待而不是无限阻塞：

```go
import "context"

// 用 buffered channel 实现的并发闸门：cap = 最大在飞请求数
type ConcurrencyGate chan struct{}

func (g ConcurrencyGate) Acquire(ctx context.Context) error {
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err() // 排队超时，快速失败返回 429
	}
}
func (g ConcurrencyGate) Release() { <-g }
```

第 3 层在前面「钩子 2」已经演示：预算耗尽时不报错，而是降级到便宜模型 + 告警。对 C 端产品，「回答质量暂时降级」几乎总是好过「服务不可用」。

## 五、踩坑清单

1. **价格表不写死在代码里**：调价一次，常量就是谎言。配置文件 + 启动加载 + 热更新三者至少做到前两个。
2. **`output_tokens` 才是成本大头**：输入价格通常只有输出的 1/4~1/5。只监控输入量会严重低估账单——尤其对话类应用，历史消息随轮数线性膨胀。
3. **记账和扣预算要是同一条数据**：预估用于拦截，结算必须用服务端 usage；两套数字混用，预算熔断一定在某个深夜失灵。
4. **限流返回 429 要告诉客户端重试时间**：`Retry-After` 头 + 明确错误体，否则前端会把限流当故障反复重试，雪上加霜。
5. **降级链要演练**：预算熔断、限流降级一年触发不了几次，真触发那天就是线上事故。评测集（第 3 篇）里应该有用便宜模型跑的基准分，降级前知道质量会掉多少。
6. **别忘了流式响应**：SSE 场景下 usage 通常在流的最后一个 chunk 才到达，记账钩子在流结束后触发，别在首个 chunk 就记。

## 六、本篇小结与下篇预告

成本治理的完整闭环：**计量**（usage 记账 + tiktoken 预估）→ **拦截**（预算熔断 + 输入长度保护）→ **限流**（令牌桶 + 并发闸门）→ **降级**（便宜模型兜底）。做完这些，账单从「月底惊吓」变成了仪表盘上的一条曲线。

但有一个更优雅的成本手段本篇特意没展开——**第 5 篇：语义缓存实战**。重复和相似的问题不该重复花钱：缓存键怎么设计、相似度阈值怎么定、命中率实测能到多少，下一篇用数据说话。

---

## 版本说明（2026-11-25 核对）

| 依赖 | 本文使用版本 | 核对来源 |
| --- | --- | --- |
| Go | 1.26+ | — |
| github.com/openai/openai-go/v3 | v3.107.0（2026-11-24） | proxy.golang.org `@latest` |
| github.com/pkoukk/tiktoken-go | v0.1.8（2025-09） | proxy.golang.org `@latest` |
| golang.org/x/time | v0.16.0（2026-08） | proxy.golang.org `@latest` |

## 相关阅读

- [LLM 应用工程化③：自动化评测集](https://blog.911015.com/ai/llm-ops-3-eval-suite.html) —— 降级链的质量底线靠它标定
- [LLM 应用工程化①：Prompt 版本化](https://blog.911015.com/ai/llm-ops-1-prompt-mgmt.html) —— Prompt 越长越贵，版本化时也该记一笔成本账
- [Go RAG 实战8 - 5 万块压测+6 款向量库横评：Go RAG 收官](https://blog.911015.com/ai/go-rag-8-vector-db-showdown.html) —— 同一套「压测换结论」方法论
- [LLM 应用工程化系列聚合目录](https://blog.911015.com/ai/llm-ops.html)

> **声明**：本文由 AI 辅助生成，代码均对照官方文档核验；如发现任何错误或不一致，欢迎在评论区指正。
