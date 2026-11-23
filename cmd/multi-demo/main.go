// 多 Agent 协作最小 demo：两个 mock agent 组成一条流水线。
// 调研员先列提纲，写手再按提纲出稿——不接大模型也能跑通，
// 用来验证 multi 包的基础抽象是否自洽。
// 运行：go run ./cmd/multi-demo
package main

import (
	"context"
	"fmt"
	"log"

	"ai-interview-coach/multi"
)

// scoutAgent 模拟"调研员"：把目标拆成调研提纲。
type scoutAgent struct{}

func (scoutAgent) Name() string { return "scout" }

func (scoutAgent) Run(_ context.Context, t multi.Task) (multi.Result, error) {
	outline := fmt.Sprintf("提纲：%s 的定义 → 主流方案对比 → 选型建议", t.Goal)
	return multi.Result{Output: outline, Summary: "调研提纲已就绪"}, nil
}

// writerAgent 模拟"写手"：按上游提纲产出结论文稿。
type writerAgent struct{}

func (writerAgent) Name() string { return "writer" }

func (writerAgent) Run(_ context.Context, t multi.Task) (multi.Result, error) {
	body := fmt.Sprintf("《%s》\n%s\n结论：先跑基准测试再定方案。", t.Goal, t.Inputs["outline"])
	return multi.Result{Output: body, Summary: "初稿已产出"}, nil
}

func main() {
	ctx := context.Background()
	scout, writer := scoutAgent{}, writerAgent{}

	// 流水线拓扑：上一棒的 Output 作为下一棒的输入
	r1, err := scout.Run(ctx, multi.Task{Goal: "Go 项目 ORM 选型"})
	if err != nil {
		log.Fatal(err)
	}
	r2, err := writer.Run(ctx, multi.Task{
		Goal:   "Go 项目 ORM 选型",
		Inputs: map[string]string{"outline": r1.Output},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(r2.Output)
}
