# 多 Agent 协作系列 · 8 篇规划（2026-11-23 定）

主线案例贯穿：第 2 篇起引入「单 Agent 硬撑的技术调研助手」，每篇重构一步；第 8 篇回到面试教练做完整多 Agent 重构。
技术红线：只用 openai-go（写文当天核对版本）+ 标准库，不引入第三方 Agent 框架；代码必须真实编译运行。

| # | 标题（预案） | 核心问题 | 配套代码 |
|---|---|---|---|
| 1 | 一个 Agent 不够？多智能体协作第一课 | 单 Agent 天花板：上下文爆炸/角色冲突/单点失败；三种基本拓扑 | `multi/` 基础接口（Task/Result/Agent）+ `cmd/multi-demo` |
| 2 | 主从架构实战：Go 手写 Orchestrator-Worker | 拆任务、派任务、收结果；任务描述词怎么写不跑偏 | `multi/orchestrator.go` + worker 池 |
| 3 | Handoff 任务交接：Agent 之间传话不丢上下文 | 何时换 Agent；交接包（summary+待办+证据）结构设计 | `multi/handoff.go` |
| 4 | 共享黑板：多 Agent 共用一份记忆 | 黑板 vs 点对点；读写冲突与写锁；条目生命周期 | `multi/blackboard.go` |
| 5 | 3 个 Agent 并行干活：并发调度与结果合并 | goroutine/errgroup；部分失败；投票/合并策略 | `multi/parallel.go` |
| 6 | 多 Agent 怎么测？轨迹评测（Trajectory Eval） | 评整条协作轨迹而非单点 prompt；评测器 + 打分 rubric | `multi/eval.go` |
| 7 | 翻车现场：死循环、超时与降级 | 编排死循环检测；超时预算；降级到单 Agent | `multi/guard.go` |
| 8 | 收官：把面试教练改成多 Agent 团队 | 出题/评估/报告三角色协作完整重构 | 复用 coach 仓库 |

标题策略：问句/数字前置 + Go/Agent 关键词锚点；禁比喻式标题（后台数据证伪）。
发布节奏：每周一/三/五 07:30 群发，写稿走备稿任务（周日/二/四晚）。
