// 多智能体系列第 6 篇 demo：轨迹评测。
// 同一条编排跑两遍：一遍全员正常，一遍混进一个"划水工人"，
// 用同一组规则打分，看评测报告如何把问题归因到具体那一棒。
package main

import (
	"context"
	"fmt"
	"time"

	"ai-interview-coach/multi"
)

type worker struct {
	name    string
	summary string // 空串模拟划水：调用成功但什么都没产出
}

func (w worker) Name() string { return w.name }

func (w worker) Run(_ context.Context, _ multi.Task) (multi.Result, error) {
	time.Sleep(20 * time.Millisecond) // 模拟真实耗时
	return multi.Result{Output: w.summary, Summary: w.summary}, nil
}

func plan(_ context.Context, goal string) ([]multi.Subtask, error) {
	return []multi.Subtask{
		{Role: "researcher", Task: multi.Task{Goal: goal}},
		{Role: "benchmarker", Task: multi.Task{Goal: goal}},
		{Role: "advisor", Task: multi.Task{Goal: goal}},
	}, nil
}

// runOnce 跑一遍完整协作并返回评测报告。
func runOnce(register func(o *multi.Orchestrator)) multi.Report {
	orch := multi.NewOrchestrator()
	register(orch)
	rec := multi.NewRecorder("Go 项目 ORM 选型")
	orch.WithRecorder(rec)

	if _, err := orch.Run(context.Background(), "Go 项目 ORM 选型", plan); err != nil {
		fmt.Println("运行失败:", err)
	}
	// 同一组规则：成功、非空产出、单棒耗时不超过 100ms
	return multi.Evaluate(rec.Trace(),
		multi.RuleAllSucceeded,
		multi.RuleNoEmptyOutput,
		multi.RuleLatencyUnder(100*time.Millisecond),
	)
}

func main() {
	fmt.Println("==== 健康运行 ====")
	healthy := runOnce(func(o *multi.Orchestrator) {
		o.Register("researcher", worker{"researcher", "候选清单 3 项"})
		o.Register("benchmarker", worker{"benchmarker", "压测报告已出"})
		o.Register("advisor", worker{"advisor", "选型结论已定"})
	})
	fmt.Printf("得分 %d，问题 %d 条\n", healthy.Score, len(healthy.Problems))

	fmt.Println("==== 划水运行：benchmarker 产出为空 ====")
	lazy := runOnce(func(o *multi.Orchestrator) {
		o.Register("researcher", worker{"researcher", "候选清单 3 项"})
		o.Register("benchmarker", worker{"benchmarker", ""}) // 划水
		o.Register("advisor", worker{"advisor", "选型结论已定"})
	})
	fmt.Printf("得分 %d\n", lazy.Score)
	for _, p := range lazy.Problems {
		fmt.Println(" -", p)
	}
}
