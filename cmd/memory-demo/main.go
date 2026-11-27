// Agent 记忆系统最小 demo：模拟面试教练的两次会话。
// 第一场面试结束后，不把几千字原始记录全塞进第二场，
// 而是沉淀出 3 条记忆；第二场开场只召回记忆做针对性出题。
// 不接大模型也能跑通，用来验证 memory 包的基础抽象是否自洽。
// 运行：go run ./cmd/memory-demo
package main

import (
	"context"
	"fmt"
	"log"

	"ai-interview-coach/memory"
)

func main() {
	ctx := context.Background()
	store := memory.NewInMemoryStore()

	// 第一场面试的原始记录（节选）：两千字里只有 3 句话值得记住
	transcript := "面试官：说说 goroutine 泄漏的常见场景？……候选人：呃，channel 没关闭会泄漏……" +
		"面试官：具体哪种没关闭？……候选人：（沉默）……（以下省略 1800 字寒暄与闲聊）"

	// 会话结束后，把原始记录沉淀为三类记忆（第 3 篇起交给 LLM 自动抽取）
	memories := []memory.Entry{
		{Kind: memory.KindEpisodic, Content: "goroutine 泄漏题：能说出 channel 未关闭，但追问具体场景时沉默", Importance: 0.9, Source: "session-1"},
		{Kind: memory.KindSemantic, Content: "目标岗位是 Go 后端，自称 3 年经验", Importance: 0.7, Source: "session-1"},
		{Kind: memory.KindProcedure, Content: "追问卡壳时先给提示，不要直接公布答案", Importance: 0.6, Source: "session-1"},
	}
	for _, m := range memories {
		if err := store.Save(ctx, m); err != nil {
			log.Fatal(err)
		}
	}

	// 对比：上下文窗口方案 vs 记忆方案的开场成本
	fmt.Printf("原始记录 %d 字 → 沉淀为 %d 条记忆\n\n", len([]rune(transcript))+1800, len(memories))

	// 第二场面试开场：召回情景+语义记忆，拼进系统提示词
	got, err := store.Recall(ctx, memory.Query{
		Kinds: []memory.Kind{memory.KindEpisodic, memory.KindSemantic},
		Limit: 5,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("第二场开场，教练记得：")
	for _, e := range got {
		fmt.Printf("  [%s|重要性%.1f] %s\n", e.Kind, e.Importance, e.Content)
	}
	fmt.Println("→ 本场出题策略：goroutine 泄漏加追问，按后端 3 年经验定难度")
}
