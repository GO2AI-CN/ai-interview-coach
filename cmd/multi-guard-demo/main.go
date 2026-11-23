// 多智能体系列第 7 篇 demo：超时、熔断与降级。
// 三个场景：超时重试后成功 / 连续失败触发熔断快速拒绝 / 降级兜底。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-interview-coach/multi"
)

// flaky 按脚本表演：第 1 次调用挂到超时，之后成功。
type flaky struct{ calls *int }

func (f flaky) Name() string { return "flaky-worker" }

func (f flaky) Run(ctx context.Context, _ multi.Task) (multi.Result, error) {
	*f.calls++
	if *f.calls == 1 {
		select {
		case <-ctx.Done(): // 尊重取消：演示超时如何切断卡死的调用
			return multi.Result{}, ctx.Err()
		case <-time.After(5 * time.Second): // 没有 Guard 时会卡死 5 秒
			return multi.Result{Summary: "慢工出细活"}, nil
		}
	}
	return multi.Result{Summary: "第 2 次尝试成功"}, nil
}

// alwaysFail 每次调用都报错。
type alwaysFail struct{}

func (alwaysFail) Name() string { return "broken-worker" }

func (alwaysFail) Run(_ context.Context, _ multi.Task) (multi.Result, error) {
	return multi.Result{}, errors.New("模型接口 500")
}

func main() {
	ctx := context.Background()

	fmt.Println("==== 场景 A：超时 100ms，重试 3 次 ====")
	calls := 0
	g1 := multi.NewGuard(flaky{&calls},
		multi.WithTimeout(100*time.Millisecond), multi.WithRetry(3))
	r, err := g1.Run(ctx, multi.Task{Goal: "调研"})
	fmt.Printf("结果=%q err=%v，实际调用次数=%d\n", r.Summary, err, calls)

	fmt.Println("==== 场景 B：连续失败 2 次熔断，冷却 500ms ====")
	g2 := multi.NewGuard(alwaysFail{},
		multi.WithRetry(1), multi.WithCircuitBreaker(2, 500*time.Millisecond))
	for i := 1; i <= 3; i++ { // 第 3 次应被熔断器快速拒绝
		_, err := g2.Run(ctx, multi.Task{Goal: "调研"})
		fmt.Printf("第 %d 次调用：%v\n", i, err)
	}

	fmt.Println("==== 场景 C：降级兜底 ====")
	g3 := multi.NewGuard(alwaysFail{},
		multi.WithRetry(1),
		multi.WithFallback(func(_ multi.Task) (multi.Result, error) {
			return multi.Result{Summary: "[降级] 基于历史报告给出参考结论"}, nil
		}))
	r, err = g3.Run(ctx, multi.Task{Goal: "调研"})
	fmt.Printf("结果=%q err=%v（错误被兜住，链路继续）\n", r.Summary, err)
}
