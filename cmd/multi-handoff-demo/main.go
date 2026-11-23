// 多智能体系列第 3 篇 demo：Handoff 交接包接力。
// 场景：技术调研助手——研究员出候选清单，压测员只凭交接包跑基准，
// 顾问再只凭上一棒交接包出选型结论。全程没有"见上文"。
package main

import (
	"context"
	"fmt"

	"ai-interview-coach/multi"
)

// 三棒各自的 ChainStep。每棒只依赖 Task 和上一棒 Handoff。

func researcher(_ context.Context, t multi.Task, _ multi.Handoff) (multi.Result, multi.Handoff, error) {
	out := "候选 3 项：fasthttp（零分配，性能最强）/ net/http+chi（标准库，生态稳）/ fiber（API 最顺手，兼容 fasthttp）"
	return multi.Result{Output: out, Summary: "[researcher] 候选清单 3 项已出"},
		multi.Handoff{
			Summary: "已圈定 3 个 Go Web 库候选，需要压测数据支撑选型",
			Todos:   []string{"对 3 个候选跑同条件 QPS 基准", "记录各自内存分配表现"},
			Evidence: map[string]string{
				"fasthttp 零分配设计":  "github.com/valyala/fasthttp README 基准章节",
				"fiber 基于 fasthttp": "github.com/gofiber/fiber 项目说明",
			},
		}, nil
}

// 关键在这一棒：压测员完全不认识"研究员"，
// 它读的是 prev.Todos 和 prev.Evidence，照单执行。
func benchmarker(_ context.Context, _ multi.Task, prev multi.Handoff) (multi.Result, multi.Handoff, error) {
	fmt.Println("---- 压测员收到的交接包 ----")
	fmt.Println(prev.String())
	out := "QPS：fasthttp 118k / fiber 112k / net/http+chi 96k；alloc：fasthttp 0.8MB，另两个 5MB+"
	return multi.Result{Output: out, Summary: "[benchmarker] 压测报告已出"},
		multi.Handoff{
			Summary: "压测完成：fasthttp 性能最优，chi 胜在标准库与生态",
			Todos:   []string{"结合团队熟悉度给出最终选型与迁移建议"},
			Evidence: map[string]string{
				"QPS 与 alloc 数据": "本地 wrk 压测输出（附在报告末尾）",
			},
		}, nil
}

func advisor(_ context.Context, t multi.Task, prev multi.Handoff) (multi.Result, multi.Handoff, error) {
	fmt.Println("---- 顾问收到的交接包 ----")
	fmt.Println(prev.String())
	out := "结论：选 net/http+chi——「" + t.Goal + "」对极限 QPS 不敏感，标准库长期维护成本最低"
	return multi.Result{Output: out, Summary: "[advisor] 选型结论已定"}, multi.Handoff{}, nil
}

func main() {
	res, err := multi.RunChain(context.Background(),
		multi.Task{Goal: "为内部 API 网关选 Web 框架"},
		[]multi.ChainStep{researcher, benchmarker, advisor})
	if err != nil {
		panic(err)
	}
	fmt.Println("---- 最终结果 ----")
	fmt.Println(res.Output)
	fmt.Println(res.Summary)
}
