// Token 用量统计：AI 应用上线后「每次面试花多少钱」是绕不开的问题。
// 做法是把每个响应的 Usage 累加进进程级计数器，会话开始时取快照、
// 结束时求差值，即得到单场消耗。计数器随日志/回复输出，重启归零。
package llm

import "sync/atomic"

// Usage 一次请求或累计的 token 用量。
type Usage struct {
	PromptTokens     int64 // 输入 token（题面 + 评估上下文，通常是开销大头）
	CompletionTokens int64 // 输出 token
	TotalTokens      int64
}

// Sub 求两次快照之间的差值（本场消耗 = 结束时 - 开始时）。
func (u Usage) Sub(start Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens - start.PromptTokens,
		CompletionTokens: u.CompletionTokens - start.CompletionTokens,
		TotalTokens:      u.TotalTokens - start.TotalTokens,
	}
}

// usageCounter 进程级累计计数器。Bot 只有一个 LLM 客户端，全局计数够用；
// 需要更精细的统计（按用户/按天）时再以它为骨架扩展。
type usageCounter struct {
	prompt     atomic.Int64
	completion atomic.Int64
}

// globalUsage 全进程累计；随日志输出，重启归零，长期数据交给日志系统。
var globalUsage usageCounter

// add 累加一次请求的用量（来自响应里的 usage 字段，供应商已经算好）。
func (c *usageCounter) add(prompt, completion int64) {
	c.prompt.Add(prompt)
	c.completion.Add(completion)
}

// TotalUsageSnapshot 返回进程启动以来的累计用量。
func TotalUsageSnapshot() Usage {
	p, c := globalUsage.prompt.Load(), globalUsage.completion.Load()
	return Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c}
}
