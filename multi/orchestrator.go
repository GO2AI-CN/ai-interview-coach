package multi

import (
	"context"
	"fmt"
	"strings"
)

// Orchestrator 实现主从（Supervisor-Worker）拓扑：
// 一个编排者 + 一组注册的 Worker。
// 编排者只做三件事——拆任务、派任务、收结果汇总，
// 自己一行产出都不写，干活全部委托出去。
type Orchestrator struct {
	workers map[string]Agent // 角色名 → 干活的 Agent
}

// NewOrchestrator 创建一个空的编排者。
func NewOrchestrator() *Orchestrator {
	return &Orchestrator{workers: map[string]Agent{}}
}

// Register 把某个角色绑定到一个 Agent。后注册覆盖先注册。
func (o *Orchestrator) Register(role string, a Agent) {
	o.workers[role] = a
}

// Subtask 是编排者拆出来的一个子任务：
// 明确指定"交给谁"（Role）和"干什么"（Task）。
type Subtask struct {
	Role string // 目标 Worker 的角色名
	Task Task   // 子任务内容
}

// PlanFunc 把总目标拆成有序子任务。
// 用函数类型而不是硬编码，是为了后面能把"规则拆解"
// 换成"让大模型来拆"，编排主流程一行不用改。
type PlanFunc func(ctx context.Context, goal string) ([]Subtask, error)

// Run 按 plan 给出的顺序，把子任务逐个派给对应 Worker，
// 汇总每个 Worker 的产出与摘要，作为整个协作的 Result。
func (o *Orchestrator) Run(ctx context.Context, goal string, plan PlanFunc) (Result, error) {
	subs, err := plan(ctx, goal)
	if err != nil {
		return Result{}, fmt.Errorf("multi: 拆任务失败: %w", err)
	}
	var outputs, summaries []string
	for i, s := range subs {
		w, ok := o.workers[s.Role]
		if !ok {
			return Result{}, fmt.Errorf("multi: 子任务 %d 引用了未注册的角色 %q", i, s.Role)
		}
		r, err := w.Run(ctx, s.Task)
		if err != nil {
			return Result{}, fmt.Errorf("multi: 角色 %q 执行失败: %w", s.Role, err)
		}
		outputs = append(outputs, r.Output)
		summaries = append(summaries, fmt.Sprintf("[%s] %s", s.Role, r.Summary))
	}
	return Result{
		Output:  strings.Join(outputs, "\n\n"),
		Summary: strings.Join(summaries, "；"),
	}, nil
}
