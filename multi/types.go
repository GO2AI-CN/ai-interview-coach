// Package multi 提供多智能体协作的基础抽象。
// 本系列后续所有协作模式（主从编排、流水线、共享黑板）
// 都建立在同一组接口上，不依赖任何第三方 Agent 框架。
package multi

import "context"

// Task 是一次协作的最小单元：目标 + 输入材料。
type Task struct {
	Goal   string            // 要完成什么，一句话说清
	Inputs map[string]string // 输入材料，如前文摘要、检索结果
}

// Result 是 Agent 干完活之后的交付物。
type Result struct {
	Output  string // 完整产出：一段分析、一份报告、一段代码
	Summary string // 一句话摘要，给编排者汇总或交给下一个 Agent
}

// Agent 是"能干活的角色"的最小接口。
// 具体实现可以是大模型调用、规则引擎，甚至一个纯函数。
type Agent interface {
	Name() string
	Run(ctx context.Context, t Task) (Result, error)
}
