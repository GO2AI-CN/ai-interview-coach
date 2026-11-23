package multi

import (
	"context"
	"fmt"
	"strings"
)

// Handoff 是 Worker 之间交接工作的"信封"。
// 设计原则只有一条：拿到 Handoff 的人不需要回头追问上一棒，
// 只看这个结构体就能独立开工。这就是"上下文自给自足"。
type Handoff struct {
	Summary  string            // 交接摘要：结论先行，一段话讲清上一棒干了什么
	Todos    []string          // 待办清单：下一棒需要完成的具体动作
	Evidence map[string]string // 证据：每条结论 → 出处（文件路径、URL、实测输出）
}

// String 把交接包渲染成可直接塞进提示词的文本块。
// 大模型对结构化文本块的利用率高，比散落的自然语言可靠得多。
func (h Handoff) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "【交接摘要】\n%s\n\n【待办】\n", h.Summary)
	for i, t := range h.Todos {
		fmt.Fprintf(&b, "%d. %s\n", i+1, t)
	}
	if len(h.Evidence) > 0 {
		b.WriteString("\n【证据】\n")
		for claim, src := range h.Evidence {
			fmt.Fprintf(&b, "- %s ← %s\n", claim, src)
		}
	}
	return b.String()
}

// ChainStep 是接力流水线里的一棒：
// 拿到上一棒的交接包，产出本次 Result 和交给下一棒的新交接包。
type ChainStep func(ctx context.Context, t Task, prev Handoff) (Result, Handoff, error)

// RunChain 把一串 ChainStep 串成接力跑。
// 每一棒只能看到：总目标 Task + 上一棒的 Handoff，
// 看不到更早的对话历史——这正是我们想要的解耦。
func RunChain(ctx context.Context, t Task, steps []ChainStep) (Result, error) {
	var (
		prev Handoff // 第一棒的输入是空交接包
		last Result
	)
	for i, step := range steps {
		r, next, err := step(ctx, t, prev)
		if err != nil {
			return Result{}, fmt.Errorf("multi: 第 %d 棒交接失败: %w", i+1, err)
		}
		prev, last = next, r
	}
	return last, nil
}
