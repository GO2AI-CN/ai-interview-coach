// Package memory 提供 Agent 记忆系统的基础抽象。
// 本系列后续所有能力（摘要压缩、事实抽取、衰减召回、遗忘 GC）
// 都建立在同一组接口上，不依赖任何第三方 Agent 框架。
package memory

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind 记忆类型：四类记忆对应不同的写入时机与生命周期。
type Kind string

const (
	KindShort     Kind = "short"     // 短期：本次会话的工作记忆，会话结束即失效
	KindEpisodic  Kind = "episodic"  // 情景：发生过的具体事件，如"goroutine 泄漏题没答上来"
	KindSemantic  Kind = "semantic"  // 语义：提炼出的事实，如"目标岗位是 Go 后端"
	KindProcedure Kind = "procedure" // 程序：怎么做事的规则，如"追问时先给提示再给答案"
)

// Entry 是一条记忆的最小单元。
// 字段设计即本系列的索引：Kind 决定存多久，Importance 和 CreatedAt 决定召回排序。
type Entry struct {
	ID         string    // 唯一标识，写入时生成
	Kind       Kind      // 记忆类型，决定生命周期与检索策略
	Content    string    // 记忆正文，必须是一句完整的话，存碎片等于没存
	Importance float64   // 重要性 0~1，写入时打分，检索时加权
	Source     string    // 来源：哪次会话、哪个 Agent 写下的，便于回溯
	CreatedAt  time.Time // 写入时间，第 4 篇时间衰减要用
}

// Query 描述一次召回的条件。
type Query struct {
	Kinds []Kind // 限定记忆类型；空表示不限
	Text  string // 检索词；本篇先精确包含匹配，语义检索留给第 4 篇
	Limit int    // 最多返回几条，0 表示不限
}

// Store 是记忆系统的最小接口：写入、按条件召回、删除。
// 实现可以是内存、SQLite 或向量库，调用方不感知。
type Store interface {
	Save(ctx context.Context, e Entry) error
	Recall(ctx context.Context, q Query) ([]Entry, error)
	Forget(ctx context.Context, id string) error
}

// InMemoryStore 是最小可用实现：一个 map + 读写锁。
// 生产实现留给后续篇章，先把读写契约定下来。
type InMemoryStore struct {
	mu      sync.RWMutex
	entries map[string]Entry
	seq     int // 自增序号，用于生成 ID
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{entries: make(map[string]Entry)}
}

func (s *InMemoryStore) Save(_ context.Context, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if e.ID == "" {
		e.ID = "mem-" + strconv.Itoa(s.seq)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	s.entries[e.ID] = e
	return nil
}

// Recall 先按类型与检索词过滤，再按时间倒序返回最新的 Limit 条。
// 排序只是占位：第 4 篇会换成 语义分×时间衰减×重要性 的三路打分。
func (s *InMemoryStore) Recall(_ context.Context, q Query) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for _, e := range s.entries {
		if len(q.Kinds) > 0 && !containsKind(q.Kinds, e.Kind) {
			continue
		}
		if q.Text != "" && !strings.Contains(e.Content, q.Text) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *InMemoryStore) Forget(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, id)
	return nil
}

func containsKind(ks []Kind, k Kind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

