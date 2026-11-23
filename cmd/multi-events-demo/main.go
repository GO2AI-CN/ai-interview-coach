// 多智能体系列第 5 篇 demo：事件驱动并行调度。
// 场景：技术调研助手——顾问不等 WaitGroup，而是订阅「压测」分区，
// 睡到 QPS 数据上板被自动叫醒；另演示一次等不到就超时退出的情况。
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ai-interview-coach/multi"
)

// hasQPS 是顾问的唤醒条件：压测分区出现带 QPS 的便签。
func hasQPS(notes []multi.Note) bool {
	for _, n := range notes {
		if strings.Contains(n.Content, "QPS") {
			return true
		}
	}
	return false
}

func main() {
	bb := multi.NewBlackboard()
	bb.Pin("调研", "researcher", "候选：fasthttp / chi / fiber")

	awake := make(chan error, 1)
	go func() { // 顾问：阻塞等待唤醒，不占 CPU
		awake <- multi.WaitFor(context.Background(), bb, "压测", hasQPS)
	}()

	time.Sleep(300 * time.Millisecond) // 压测员还在跑基准
	fmt.Println("（300ms 里顾问在睡觉，主 goroutine 也没轮询）")
	bb.Pin("压测", "benchmarker", "QPS：fasthttp 118k / chi 96k")
	if err := <-awake; err == nil {
		fmt.Println("顾问被事件叫醒：QPS 数据已上板")
	}

	// 反例：等一个永远不会来的条件，ctx 超时兜底
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := multi.WaitFor(ctx, bb, "上线报告", func([]multi.Note) bool { return false })
	fmt.Printf("等不到的事件，ctx 收场：%v\n", err)
}
