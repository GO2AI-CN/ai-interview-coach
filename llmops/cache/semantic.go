// Package cache 实现《LLM 应用工程化》系列第 5 篇的语义缓存。
package cache

import (
	"math"
	"sync"
	"time"
)

// Key 缓存检索键：Embedding + 模型 + Prompt 版本三重隔离（防串味）
type Key struct {
	Embedding []float32
	Model     string
	PromptVer string
}

// Entry 缓存条目
type Entry struct {
	Key       Key
	Question  string // 保留原文，便于排查错命中
	Answer    string
	CreatedAt time.Time
	TTL       time.Duration
}

// Expired 是否过期
func (e *Entry) Expired() bool { return time.Since(e.CreatedAt) > e.TTL }

// SemanticCache 内存语义缓存；条目规模到十万级再考虑向量索引升级
type SemanticCache struct {
	mu        sync.RWMutex
	entries   []Entry
	threshold float32
	Hits      int
	Misses    int
}

func NewSemanticCache(threshold float32) *SemanticCache {
	return &SemanticCache{threshold: threshold}
}

// cosine 余弦相似度
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

// Get 检索最相似条目；返回近似度便于观测「接近阈值」的 miss
func (c *SemanticCache) Get(k Key) (*Entry, float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *Entry
	var bestSim float32
	for i := range c.entries {
		e := &c.entries[i]
		if e.Expired() || e.Key.Model != k.Model || e.Key.PromptVer != k.PromptVer {
			continue
		}
		if s := cosine(k.Embedding, e.Key.Embedding); s > bestSim {
			bestSim, best = s, e
		}
	}
	if best != nil && bestSim >= c.threshold {
		c.Hits++
		return best, bestSim, true
	}
	c.Misses++
	return nil, bestSim, false
}

// Set 写入缓存
func (c *SemanticCache) Set(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

// HitRate 命中率统计
func (c *SemanticCache) HitRate() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.Hits + c.Misses
	if total == 0 {
		return 0
	}
	return float64(c.Hits) / float64(total)
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
