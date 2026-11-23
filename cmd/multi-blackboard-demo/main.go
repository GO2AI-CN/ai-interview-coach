// 多智能体系列第 4 篇 demo：黑板模式。
// 场景：技术调研助手——研究员和压测员并发干活、随时围观彼此进度，
// 顾问不看任何"交接"，只读整块黑板就出汇总结论。
package main

import (
	"fmt"
	"sync"

	"ai-interview-coach/multi"
)

func main() {
	bb := multi.NewBlackboard()
	var wg sync.WaitGroup

	// 研究员：边调研边把中间结论贴上黑板。
	wg.Add(1)
	go func() {
		defer wg.Done()
		bb.Pin("调研", "researcher", "候选：fasthttp / net/http+chi / fiber")
		bb.Pin("调研", "researcher", "fiber 与 fasthttp 同源，二选一即可")
	}()

	// 压测员：不等调研"交卷"，先读黑板上的候选名单就开跑；
	// 测完把数据贴回"压测"分区，研究员随时能看到。
	wg.Add(1)
	go func() {
		defer wg.Done()
		candidates := bb.Section("调研") // 可能只有部分结论，看到了多少用多少
		fmt.Printf("压测员开局读到 %d 条调研便签\n", len(candidates))
		bb.Pin("压测", "benchmarker", "QPS：fasthttp 118k / fiber 112k / chi 96k")
		bb.Pin("压测", "benchmarker", "alloc：fasthttp 0.8MB，另两个 5MB+")
	}()

	wg.Wait() // 两人并发干完，黑板上各有产出

	// 顾问：不认识任何一棒的"上下文"，只读黑板快照。
	fmt.Println("---- 黑板快照 ----")
	fmt.Println(bb.Snapshot())
	bb.Pin("结论", "advisor", "选 net/http+chi：网关场景对极限 QPS 不敏感，维护成本最低")
	fmt.Println("---- 最终结论分区 ----")
	for _, n := range bb.Section("结论") {
		fmt.Printf("[%s] %s\n", n.Author, n.Content)
	}
}
