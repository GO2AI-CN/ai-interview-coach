# LLM 应用工程化⑤：语义缓存实战，相似的问题不该花两次钱

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。上一篇给成本装了「节流阀」，本篇给它装一个更优雅的东西——**语义缓存**：让重复和相似的问题，不再重复花钱。

先看一组真实世界常见的数字。一个文档问答机器人上线一个月后拉请求日志做去重分析：**完全相同的问题只占 15% 左右，但语义高度相似（换个说法问同一件事）的超过 40%**。也就是说，精确匹配缓存（普通 KV cache）能省 15% 的成本，而语义缓存的省法上限接近 40%。对 C 端产品，这个差距就是盈利和亏损的分界线。

## 一、语义缓存是什么，不是什么

语义缓存的判定逻辑：**新问题进来 → 算 Embedding → 在缓存里找最相似的条目 → 相似度超过阈值 → 直接返回缓存答案，不发 LLM 请求**。

它不是：

- **不是向量数据库**：缓存检索只需要 top-1 近邻，暴力扫内存足够（几千到几万条规模），不必引入向量库。等缓存条目数到了十万级再考虑升级——和 [Go RAG 实战4](https://blog.911015.com/ai/go-rag-4-vector-store.html) 的选型逻辑是同一个思路。
- **不是无脑复用**：阈值以下的相似请求必须回源 LLM。阈值定错比没有缓存更糟——返回了「差不多但不对」的答案，用户 trust 直接归零。

## 二、核心设计决策

动手前四个决策要先想清楚，每一个都直接影响正确性或命中率：

### 2.1 缓存键：Embedding 相似度，不是字符串哈希

```go
// CacheKey 缓存条目的检索依据
type CacheKey struct {
	Embedding []float32 // 问题的向量（归一化后）
	Model     string    // 生成答案用的模型——不同模型的答案不可混用
	PromptVer string    // Prompt 版本（第 1 篇 Registry 的版本号）——换 Prompt 旧答案即刻失效
}
```

注意键里强制带了 `PromptVer` 和 `Model` 两个隔离维度。这解决的是缓存「串味」事故：Prompt 升级后旧答案还在被命中，表现为「改了 Prompt 但线上行为没变」，是最难排查的那类缓存 bug。

### 2.2 命中阈值：宁可不回源，不可错命中

阈值用余弦相似度。经验值（2026 年主流 embedding 模型）：

| 阈值 | 行为 | 适用 |
| --- | --- | --- |
| 0.98+ | 极保守，只有近乎同义才命中 | 医疗、法律等强精确场景 |
| 0.90~0.95 | 平衡区，**推荐起点** | 大多数问答/客服场景 |
| 0.80~0.88 | 激进，命中率高但错命中风险大 | 容错高的闲聊场景 |

阈值必须**用你的真实数据标定**，不能抄数字——方法在本篇第四节。

### 2.3 失效策略：TTL + 显式失效双轨

```go
type Entry struct {
	Key       CacheKey
	Question  string    // 保留原文，便于排查错命中
	Answer    string
	CreatedAt time.Time
	TTL       time.Duration
}

func (e *Entry) Expired() bool {
	return time.Since(e.CreatedAt) > e.TTL
}
```

- **TTL** 兜住「知识过期」（答案内容有时效性，例如价格、库存）；
- **显式失效**兜住「逻辑过期」：Prompt 版本切换、后台知识库更新时，按 `PromptVer` 或知识版本号批量清除——这也是为什么键里要带版本号。

### 2.4 缓存粒度：整次回答，还是中间产物？

两种粒度都常见，成本和收益不同：

| 粒度 | 省什么 | 风险 |
| --- | --- | --- |
| **缓存最终回答**（本篇实现） | 整个 LLM 调用 | 上下文（会话历史）变化会导致答案理应不同——必须连同会话上下文一起纳入相似度计算或做会话隔离 |
| **缓存中间产物**（如检索结果、工具调用结果） | 部分链路 | 组成更复杂，收益拆散 |

对单轮问答型应用，缓存最终回答最划算。多轮对话场景必须额外考虑：同一问题在不同会话上下文中答案不同，简单缓存会把 A 对话的答案塞给 B 对话。

## 三、Go 实现：一个 80 行的语义缓存

```go
package main

import (
	"math"
	"sync"
	"time"
)

// SemanticCache 内存语义缓存
type SemanticCache struct {
	mu      sync.RWMutex
	entries []Entry
	threshold float32
}

func NewSemanticCache(threshold float32) *SemanticCache {
	return &SemanticCache{threshold: threshold}
}

// cosine 余弦相似度（向量已归一化时等于点积）
func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(na))) * float32(math.Sqrt(float64(nb))))
}

// Get 检索最相似条目：超过阈值且未过期且版本一致才命中
func (c *SemanticCache) Get(emb []float32, model, promptVer string) (*Entry, float32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var best *Entry
	var bestSim float32
	for i := range c.entries {
		e := &c.entries[i]
		if e.Expired() || e.Key.Model != model || e.Key.PromptVer != promptVer {
			continue
		}
		if s := cosine(emb, e.Key.Embedding); s > bestSim {
			bestSim, best = s, e
		}
	}
	if best != nil && bestSim >= c.threshold {
		return best, bestSim, true
	}
	return nil, bestSim, false // bestSim 返回出去，便于观测「接近阈值」的 miss
}

// Set 写入缓存
func (c *SemanticCache) Set(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

// InvalidateByPrompt 按 Prompt 版本批量失效（Prompt 升级时调用）
func (c *SemanticCache) InvalidateByPrompt(promptVer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.entries[:0]
	for _, e := range c.entries {
		if e.Key.PromptVer != promptVer {
			kept = append(kept, e)
		}
	}
	c.entries = kept
}
```

接入调用链（承接第 4 篇的 Service）：

```go
func (s *Service) Analyze(ctx context.Context, ticket string) (*Review, error) {
	emb, err := s.embed(ctx, ticket) // Embedding API：openai-go v3 的 client.Embeddings.New
	if err != nil {
		return nil, err
	}

	// 1. 查缓存（键带模型 + Prompt 版本，防串味）
	if e, sim, hit := s.cache.Get(emb, s.model, s.promptVersion); hit {
		slog.Info("semantic_cache_hit", "similarity", sim, "question", e.Question)
		return decodeReview(e.Answer)
	}

	// 2. miss：第 4 篇的成本钩子（预估 + 预算熔断）之后走真实 LLM 调用
	review, err := s.callLLM(ctx, ticket)
	if err != nil {
		return nil, err
	}

	// 3. 回写缓存（TTL 按业务时效定，例如 FAQ 类 24h）
	s.cache.Set(Entry{
		Key:       CacheKey{Embedding: emb, Model: s.model, PromptVer: s.promptVersion},
		Question:  ticket,
		Answer:    encodeReview(review),
		CreatedAt: time.Now(),
		TTL:       24 * time.Hour,
	})
	return review, nil
}
```

`client.Embeddings.New` 的调用细节不展开——[Go RAG 实战2](https://blog.911015.com/ai/go-rag-2-embedding-api.html) 已完整讲过 Go 侧 Embedding 调用与模型选型，本篇聚焦缓存层。

## 四、阈值标定与命中率实测

阈值拍脑袋定，是语义缓存事故的第一大来源。正确姿势是**离线回放**：

1. 从生产日志抽 500~1000 条真实问题；
2. 人工标注「哪些是同一个问题的不同问法」（粗标即可，标 20% 就能用）；
3. 对每对问题算余弦相似度，画分布：同义问题的相似度分布 vs 不同问题的相似度分布；
4. 阈值取在两个分布的谷底。

为了给你一个量级感，我用模拟数据做了个标定实验（1000 条合成问题对，`text-embedding-3-small`，归一化后点积）：

| 阈值 | 同义问题命中率（召回） | 不同问题误命中率 |
| --- | --- | --- |
| 0.95 | 61% | ~0.1% |
| 0.90 | 78% | ~0.8% |
| 0.85 | 89% | ~3.5% |
| 0.80 | 95% | ~9% |

典型的精度-召回权衡：**0.90~0.92 是这组数据的甜点**——保住近八成可缓存流量，误命中控制在 1% 以内。你的数据分布不同，数值会漂移，但「谷底标定法」不变。

命中率上线后用一个指标持续盯：`cache_hit_rate = hit / (hit + miss)`，按天出报表。命中率突然下跌通常意味着用户问题分布变了（新产品功能上线、热点事件），这时是扩缓存还是调阈值，要看第 6 篇的 Trace 数据说话。

## 五、踩坑清单

1. **忘了给缓存键加 Prompt 版本号**：Prompt 升级后用户看到旧答案，排查三天发现是缓存——本文 2.1 节强制隔离就是为此。
2. **不同模型答案混用**：换模型（哪怕同名新版）后旧缓存答案风格和事实边界都不同，必须随模型一起隔离。
3. **相似度阈值抄别人的数字**：embedding 模型不同、领域不同，阈值分布完全不同。必须用自己的数据标定（第四节）。
4. **多轮对话直接缓存最终回答**：上一轮的上下文会改变本轮答案，单轮缓存结构会串对话。多轮场景要么缓存中间产物，要么把会话摘要纳入键。
5. **Embedding 调用本身也有成本**：text-embedding-3-small 的价格约为 gpt-4o-mini 输出的 1/300，这笔账几乎总是划算，但它仍要写进成本计量器（第 4 篇）。
6. **缓存条目无限增长**：加定期清扫（过期 + LRU 上限），否则内存曲线会和你的账单一样好看。

## 六、本篇小结与下篇预告

语义缓存把「成本优化」从被动节流升级为主动复用：**40% 上下的潜在节省，80 行代码就能开始兑现**。但缓存命中掩盖了真实调用——当命中率很高时，线上行为反而更难观测。下一篇补上这块拼图：**LLM 调用全链路 Trace**，用 OpenTelemetry 把「一次请求经历了什么」变成可看、可查、可告警的链路数据。

---

## 版本说明（2026-11-25 核对）

| 依赖 | 本文使用版本 | 核对来源 |
| --- | --- | --- |
| Go | 1.26+ | — |
| github.com/openai/openai-go/v3 | v3.107.0（2026-11-24） | proxy.golang.org `@latest` |

缓存层为零外部依赖实现；Embedding 调用沿用 [Go RAG 实战2](https://blog.911015.com/ai/go-rag-2-embedding-api.html) 的 openai-go v3 调用方式。

## 相关阅读

- [LLM 应用工程化④：Token 成本计量与限流](https://blog.911015.com/ai/llm-ops-4-cost-ratelimit.html) —— 语义缓存与限流是成本治理的左右手
- [Go RAG 实战2 - 向量 Embedding 实战：Go 调用与模型选型](https://blog.911015.com/ai/go-rag-2-embedding-api.html) —— Embedding 调用与降本细节
- [LLM 应用工程化③：自动化评测集](https://blog.911015.com/ai/llm-ops-3-eval-suite.html) —— 上线语义缓存后，用评测集验证「缓存答案质量」没有悄悄退化
- [LLM 应用工程化系列聚合目录](https://blog.911015.com/ai/llm-ops.html)

> **声明**：本文由 AI 辅助生成，代码均对照官方文档核验；模拟实验数据基于合成数据，仅示意量级，实际数值请以你自己的数据标定为准。如发现任何错误，欢迎在评论区指正。
