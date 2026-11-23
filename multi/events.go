package multi

import (
	"context"
	"sync"
)

// Subscription 是黑板某个分区的一次订阅：
// 分区有新便签时，C() 会收到一个信号（不携带内容，自己去读）。
type Subscription struct {
	bb      *Blackboard
	section string
	ch      chan struct{} // 缓冲为 1，信号可缓存不阻塞
	once    sync.Once
}

// C 返回订阅信号通道。
func (s *Subscription) C() <-chan struct{} { return s.ch }

// Close 退订，之后不再收到该分区的信号。
func (s *Subscription) Close() {
	s.once.Do(func() { s.bb.unsubscribe(s.section, s.ch) })
}

// Subscribe 订阅黑板某个分区的新便签通知。
func (b *Blackboard) Subscribe(section string) *Subscription {
	sub := &Subscription{bb: b, section: section, ch: make(chan struct{}, 1)}
	b.mu.Lock()
	b.subs[section] = append(b.subs[section], sub.ch)
	b.mu.Unlock()
	return sub
}

// unsubscribe 从订阅表里摘掉某个通道。
func (b *Blackboard) unsubscribe(section string, ch chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.subs[section]
	for i, c := range list {
		if c == ch {
			b.subs[section] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// WaitFor 事件驱动地等待某分区满足条件 cond：
// 先检查一次现状，不满足就阻塞等通知，被叫醒后再查，直到满足或 ctx 取消。
// 等待期间不占 CPU，超时由调用方通过 ctx 控制。
func WaitFor(ctx context.Context, bb *Blackboard, section string, cond func([]Note) bool) error {
	sub := bb.Subscribe(section)
	defer sub.Close()
	for {
		if cond(bb.Section(section)) { // 先查现状：避免"贴完才订阅"漏掉
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err() // 超时/取消，把责任交还给调用方
		case <-sub.C():
		}
	}
}
