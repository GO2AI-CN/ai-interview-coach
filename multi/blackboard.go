package multi

import (
	"fmt"
	"strings"
	"sync"
)

// Note 是贴在黑板上的一个便签：谁写的、写了什么。
type Note struct {
	Author  string // 写便签的 Agent 名
	Content string // 便签内容，一条结论或一条数据
}

// Blackboard 是 Agent 们共享的一块黑板。
// 与 Handoff 的"点对点交接"不同，黑板是"广播式"的：
// 谁有结论谁就贴一张便签上去，谁需要信息就来读对应分区。
// 没有任何一个 Agent 是全知的，黑板本身就是公共记忆。
type Blackboard struct {
	mu    sync.RWMutex
	board map[string][]Note   // 分区名 → 该分区的便签列表
	subs  map[string][]chan struct{} // 分区名 → 订阅者信号通道
}

// NewBlackboard 创建一块空黑板。
func NewBlackboard() *Blackboard {
	return &Blackboard{
		board: map[string][]Note{},
		subs:  map[string][]chan struct{}{},
	}
}

// Pin 往指定分区贴一张便签，并发安全，多个 Agent 可同时写。
// 写完后向该分区的所有订阅者发信号（非阻塞，信号可缓存）。
func (b *Blackboard) Pin(section, author, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.board[section] = append(b.board[section], Note{Author: author, Content: content})
	for _, ch := range b.subs[section] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Section 返回某个分区的全部便签（副本），读不阻塞写。
func (b *Blackboard) Section(name string) []Note {
	b.mu.RLock()
	defer b.mu.RUnlock()
	src := b.board[name]
	out := make([]Note, len(src))
	copy(out, src)
	return out
}

// Snapshot 把整块黑板渲染成文本，适合直接喂给汇总 Agent。
func (b *Blackboard) Snapshot() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var sb strings.Builder
	for section, notes := range b.board {
		fmt.Fprintf(&sb, "【%s】\n", section)
		for _, n := range notes {
			fmt.Fprintf(&sb, "- [%s] %s\n", n.Author, n.Content)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
