# 任务总派错人？Go 手写一个多 Agent 编排者

上一篇把「技术调研助手」交给了单个 Agent：查资料、跑压测、写建议，全是一个人干。任务一复杂它就左右互搏——压测到一半回头补调研，调研笔记又把压测结论冲掉了。这一篇给它配一个领导：手写一个 Orchestrator，让它只管拆任务、派任务、收结果。配套代码在开源仓库 [ai-interview-coach](https://github.com/GO2AI-CN/ai-interview-coach) 的 `multi/orchestrator.go`。

## 一、编排者第一件事：自己不干活

主从架构最容易犯的错，是让编排者"顺便"也写两行分析。一旦开了这个头，编排者的系统提示词会迅速膨胀成第二个全能 Agent，主从架构退化成单 Agent。这和带团队一个道理：经理下场写了代码，就再也顾不上分工和验收了。

所以先立规矩：**编排者只做三件事——拆任务、派任务、收结果汇总**。落到代码上，就是一个注册表加一个 Run：

```go
// Orchestrator 实现主从（Supervisor-Worker）拓扑：
// 编排者只做三件事——拆任务、派任务、收结果汇总，
// 自己一行产出都不写，干活全部委托出去。
type Orchestrator struct {
	workers map[string]Agent // 角色名 → 干活的 Agent
}

func (o *Orchestrator) Register(role string, a Agent) {
	o.workers[role] = a
}
```

`workers` 是一个角色名到 Agent 的映射。编排者通过角色名找工人，而不是直接持有 Agent 引用——这一步解耦是后面所有扩展（换实现、加工人、动态路由）的前提。

## 二、拆任务：先定义"子任务长什么样"

派活之前，得先有活。我们定义一个 `Subtask`，把"交给谁"和"干什么"写在一起：

```go
// Subtask 是编排者拆出来的一个子任务：
// 明确指定"交给谁"（Role）和"干什么"（Task）。
type Subtask struct {
	Role string // 目标 Worker 的角色名
	Task Task   // 子任务内容（Goal + Inputs）
}
```

注意这里复用了上一篇的 `Task`（Goal + Inputs），没有新造类型。协作框架里类型越少，Agent 之间传话时丢信息的机会越小。

拆法本身做成一个函数类型：

```go
// PlanFunc 把总目标拆成有序子任务，返回的顺序就是执行顺序。
// 用函数类型而不是硬编码，是为了后面能把"规则拆解"
// 换成"让大模型来拆"，编排主流程一行不用改。
// 规则版实现见 cmd/multi-orchestrator-demo 的 planRules；
// LLM 版实现只需要返回同样形状的 []Subtask。
type PlanFunc func(ctx context.Context, goal string) ([]Subtask, error)
```

为什么强调函数类型？这是本篇最重要的一次决策。现在先用规则写死拆解逻辑，第 3 篇之后会把它换成大模型拆解——届时只需要新写一个 `PlanFunc` 实现（内部用 openai-go v3.89.0 调模型），`Orchestrator.Run` 一行都不用动。

## 三、派任务与收结果：一个循环两个防线

Run 的主循环很朴素：按 plan 顺序逐个派发，把每个工人的产出和摘要攒起来：

```go
for i, s := range subs {
	w, ok := o.workers[s.Role]
	if !ok {
		return Result{}, fmt.Errorf("multi: 子任务 %d 引用了未注册的角色 %q", i, s.Role)
	}
	r, err := w.Run(ctx, s.Task)
	if err != nil {
		return Result{}, fmt.Errorf("multi: 角色 %q 执行失败: %w", s.Role, err)
	}
	outputs = append(outputs, r.Output)
	summaries = append(summaries, fmt.Sprintf("[%s] %s", s.Role, r.Summary))
}
```

循环里埋了两条防线，都是多 Agent 系统特有的坑。第一条：**角色未注册直接报错**，绝不允许静默跳过一个子任务——少一环的调研报告比没有报告更害人，因为它看起来是完整的。第二条：**工人失败立刻中断并带上下文**（哪个角色、第几个子任务、原始错误），后面排查死循环、超时降级（第 7 篇）全靠这个错误链。

还有一个容易忽略的细节：顺序是 plan 给的，但派发是**串行**的。主从架构的第一课先不引入并发——编排者要看到前一步的结果才能确认下一步没派错人，串行是最诚实的起步姿势。等到角色之间确实没有依赖了，再把无关联的工人扔进 goroutine（第 5 篇并行调度），那时候错误处理反而要回来看这条错误链够不够清楚。

最终返回把 `outputs` 拼接成完整报告、`summaries` 拼成一行流水账——和上一篇的约定一致：`Output` 是交付物，`Summary` 是排障和汇总的抓手。

## 四、demo：三工人接到一个真任务

demo 里注册三个 mock 工人——调研员给候选清单，压测员出基准数据，架构顾问综合下结论；`planRules` 用规则把"Go 项目 ORM 选型"拆成三步：

```go
func planRules(_ context.Context, goal string) ([]multi.Subtask, error) {
	return []multi.Subtask{
		{Role: "researcher", Task: multi.Task{Goal: goal}},
		{Role: "benchmarker", Task: multi.Task{
			Goal: goal, Inputs: map[string]string{"candidates": "见上一步调研产出"},
		}},
		{Role: "advisor", Task: multi.Task{
			Goal: goal, Inputs: map[string]string{"research": "见调研", "benchmark": "见压测"},
		}},
	}, nil
}
```

`go run ./cmd/multi-orchestrator-demo` 输出一行汇总：`[researcher] 候选清单 3 项；[benchmarker] 压测报告已出；[advisor] 选型结论已定`。

眼尖的读者会发现：demo 里压测员的输入写的是"见上一步调研产出"这种占位符，并没有真的把调研的 `Result` 传过去。**这是故意的留丑**——子任务之间的传话（哪些该传全文、哪些只传摘要）正是第 3 篇 Handoff 要解决的完整问题，本篇先把编排骨架立稳。演示真实传话之前的半成品状态，比伪装成能跑更诚实。

## 五、派活描述怎么写，工人才不跑偏

主从架构失败的第一大原因不是代码 bug，是**拆出来的子任务描述本身模糊**。同一条拆任务清单，记住三个原则：

- **每个子任务只验收一个产出物**："调研 ORM"不合格，"给出 3 个候选 + 各自一句优缺点"才合格。产出物说得越具体，工人跑偏空间越小。
- **输入只给该工人需要的材料**：压测员不需要看调研报告全文，给它候选名单就够了。塞无关材料等于花 token 帮它走神。
- **子任务之间用关键词对账**：三步流水里，"压测"子任务的输入键 `candidates`、上一步产出的角色 `researcher`，这些名字本身就是契约，写 plan 时对一遍能提前发现断链。

| 检查项 | 合格写法 | 跑偏写法 |
|---|---|---|
| 产出物 | 给出 3 个候选 + 各自一句优缺点 | 调研一下 ORM |
| 输入材料 | 只传候选名单（candidates） | 传全文调研报告 |
| 交接对账 | 输入键与上一步角色名一致 | 凭感觉接活 |
| 失败处理 | 明确报错并中断 | 静默跳过该步 |

这张表建议收藏，写完每个 `PlanFunc` 都对一遍。后面把 planRules 换成大模型拆解时，同样用这张表去审模型拆出来的子任务——规则变了，标准不变。

## 写在最后

这一篇把主从架构的骨架立了起来：编排者零产出、子任务强类型、Run 主循环带两道防线。代码量不大，但"拆派发收"的职责切分是后面流水线、黑板、并行调度（第 5 篇）共同的底层。下一篇解决本篇故意留下的丑：Agent 之间怎么传话，才能让压测员真的拿到调研员的产出，而不是一个占位符？答案叫 Handoff。

如果这篇文章对你有启发，欢迎点赞、在看、转发，关注「Go2AI之路」，追更整个系列。

---
*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
