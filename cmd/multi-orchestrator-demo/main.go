// 主从架构 demo：一个 Orchestrator + 三个 mock Worker，
// 把"Go 项目 ORM 选型"拆成 调研→压测→建议 三个子任务逐个派发。
// 不接大模型也能跑通，用来验证编排主流程是否自洽。
// 运行：go run ./cmd/multi-orchestrator-demo
package main

import (
	"context"
	"fmt"
	"log"

	"ai-interview-coach/multi"
)

// researcher 模拟调研员：产出候选方案对比。
type researcher struct{}

func (researcher) Name() string { return "researcher" }

func (researcher) Run(_ context.Context, t multi.Task) (multi.Result, error) {
	out := fmt.Sprintf("候选：gorm（生态最全）/ ent（类型安全）/ sqlc（SQL 生成代码）——目标：%s", t.Goal)
	return multi.Result{Output: out, Summary: "候选清单 3 项"}, nil
}

// benchmarker 模拟压测员：对输入里的候选跑基准测试。
type benchmarker struct{}

func (benchmarker) Name() string { return "benchmarker" }

func (benchmarker) Run(_ context.Context, t multi.Task) (multi.Result, error) {
	out := fmt.Sprintf("基准：单次查询 gorm 1.2μs / ent 0.9μs / sqlc 0.4μs。候选：%s", t.Inputs["candidates"])
	return multi.Result{Output: out, Summary: "压测报告已出"}, nil
}

// advisor 模拟架构顾问：综合调研与压测给结论。
type advisor struct{}

func (advisor) Name() string { return "advisor" }

func (advisor) Run(_ context.Context, t multi.Task) (multi.Result, error) {
	out := fmt.Sprintf("建议：CRUD 为主选 gorm，性能敏感选 sqlc。依据：\n%s\n%s",
		t.Inputs["research"], t.Inputs["benchmark"])
	return multi.Result{Output: out, Summary: "选型结论已定"}, nil
}

// planRules 规则版拆解器：先把大目标拆成固定三步流水线。
// 之后篇会把它换成"大模型拆解"，编排主流程不变。
func planRules(_ context.Context, goal string) ([]multi.Subtask, error) {
	return []multi.Subtask{
		{Role: "researcher", Task: multi.Task{Goal: goal}},
		{Role: "benchmarker", Task: multi.Task{
			Goal:   goal,
			Inputs: map[string]string{"candidates": "见上一步调研产出"},
		}},
		{Role: "advisor", Task: multi.Task{
			Goal:   goal,
			Inputs: map[string]string{"research": "见调研", "benchmark": "见压测"},
		}},
	}, nil
}

func main() {
	ctx := context.Background()
	orch := multi.NewOrchestrator()
	orch.Register("researcher", researcher{})
	orch.Register("benchmarker", benchmarker{})
	orch.Register("advisor", advisor{})

	// 注意：真实代码里这里要把前一步 Result 喂给下一步，
	// demo 为了聚焦编排骨架先用占位输入，第 3 篇 Handoff 解决传话问题。
	r, err := orch.Run(ctx, "Go 项目 ORM 选型", planRules)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("=== 汇总 ===")
	fmt.Println(r.Summary)
}
