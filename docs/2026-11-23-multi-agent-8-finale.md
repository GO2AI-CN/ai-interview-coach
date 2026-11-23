# 三 Agent 搭一个面试教练：多智能体协作收官

八篇走到这里，多 Agent 骨架已经齐了：编排、交接、黑板、调度、评测、容错。但骨架终究不是产品——就像造好了发动机、底盘、变速箱，还没装上车厢。

收官篇做最后一步组装：用三个 Agent 搭一个**AI 模拟面试教练**。面试官按岗位能力模型出题，追问教练判断答案深浅动态深挖，评估师最后输出结构化评分报告。这也是本仓库最早的原点——第一系列的面试教练产品，现在用多智能体视角重做一遍。

## 任务拆解：一个教练，三个角色

单 Agent 也能做面试教练，但三个角色挤在一个提示词里会互相干扰：面试官需要中立，追问教练需要挑剔，评估师需要公允。拆开之后，每个 Agent 的提示词只做一件事，质量上限立刻不同。

| 角色 | 职责 | 复用的原语 |
| --- | --- | --- |
| 面试官 interviewer | 按岗位 rubric 生成题目 | Task/Result 基础抽象 |
| 追问教练 prober | 评估答案深浅，决定深挖或放行 | Handoff 传话（Q+A 进 Inputs） |
| 评估师 grader | 按维度打分、输出报告 | 结构化输出 + 轨迹评测 |

外加两件横切装备：每个 Agent 都套 **Guard**（超时/重试/熔断/降级四保险丝），全程挂 **Recorder** 留轨迹。这些不是收官篇的新代码，是前六篇写好的原语直接拿来用。

## LLM 抽象：脚本与真实模型只差一个实现

收官 demo 要能离线跑，关键是把「大脑」抽象成接口。`scriptedLLM` 按系统提示词的类型关键词返回脚本答案，`coachAgent` 只跟接口打交道——生产环境把 `Complete` 换成 openai-go 的 chat completions 调用，其余代码一行不改：

```go
// LLM 是 Agent 背后的大脑抽象。
type LLM interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// coachAgent 把 Task 包装成 system+user 消息交给 LLM，是多 Agent 共用的骨架。
func (c coachAgent) Run(ctx context.Context, t multi.Task) (multi.Result, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", t.Goal)
	for k, v := range t.Inputs { // Handoff 的传话内容从这里进提示词
		fmt.Fprintf(&b, "[%s]\n%s\n", k, v)
	}
	out, err := c.llm.Complete(ctx, c.system, b.String())
	if err != nil {
		return multi.Result{}, err
	}
	return multi.Result{Output: out, Summary: fmt.Sprintf("[%s] 完成", c.name)}, nil
}
```

这个 `Complete` 接口就是本仓库接真实模型的位置。顺便说，三个角色的系统提示词在 demo 里是真实存在的字符串——「按岗位能力模型（rubric）出题」「评估答案深度，答得浅就追问」——换成真模型时直接复用，这正是脚本模式的价值：提示词工程在接模型之前就完成了。

## 主循环：动态编排为什么不走静态 plan

细心的读者会发现：这个面试流程**没有用 Orchestrator**。原因是追问次数在运行前不确定——候选人答得浅就多问一轮，答到位就跳过。静态 PlanFunc 表达不了这种动态循环，所以主流程直接手写，但每一棒调用的仍然是标准 `Agent` 接口，且全部套了 Guard：

```go
for _, q := range questions {
	fmt.Println("【面试官】", q)
	answer := answers[qid]
	dec, err := prober.Run(ctx, multi.Task{
		Goal:   "判断是否需要追问",
		Inputs: map[string]string{"问题": q, "答案": answer},
	})
	if followUp, ok := strings.CutPrefix(dec.Output, "DECISION: follow_up\n"); ok {
		fmt.Println("【追问教练】", followUp) // 答得浅，深挖一轮
	} else {
		// 答到位，记入问答记录，进入下一题
	}
}
```

这是收官篇想传递的最重要的工程判断：**模式是工具不是教条**。Orchestrator 适合步骤确定的流水线；步骤由运行结果决定的动态流程，手写循环加标准接口是更诚实的选择。追问教练的决策通过 `DECISION:` 前缀返回，这就是第 6 篇说过的「结构化输出约束」——不问模型「你怎么看」，让它从固定格式里选。

这里还有个提示词细节值得展开：追问教练拿到的不只是答案，还有完整题目，打包成 `Inputs` 里的键值对。键名「问题」「答案」就是给大模型的路标——同样的内容，散装进提示词和按标签分装，模型提取关键信息的准确率完全不同。这套 `[键名]` 包裹的格式，和第 3 篇 Handoff 的渲染格式是同一个约定，全系列一以贯之。

## 容错与评测：内建而不是外挂

组装时最容易犯的错是把容错和评测当「上线前再补」的东西。收官 demo 里它们从第一行就在：三个 Agent 全部经 `guard()` 统一包裹——大模型接口超时 5 秒切断、瞬时错误重试、连续失败熔断、彻底失败降级到缓存题库；`must()` 辅助函数每跑完一棒就留一段 Span，最后拿第 6 篇的规则给这次协作打分。

## 实测：一次完整的模拟面试

demo 的候选人是脚本扮演的：第一题「channel 就是用来传数据的」答得很浅，第二题给了完整作答。运行输出：

```text
【面试官】 1. 介绍一下 channel 的底层实现
【候选人】 channel 就是用来在 goroutine 之间传数据的
【追问教练】 追问：能具体说说 hchan 结构体里发送队列和接收队列的关系吗？
【候选人】（沉默……没答上来）
【面试官】 2. context 取消是如何传播的
【候选人】 context 取消会从父节点往子节点传播……（完整作答）
==== 评分报告 ====
沟通能力 85 | 专业深度 60（channel 细节缺失，被追问后仍答不出）| 工程经验 70
协作轨迹自评：100 分，4 段轨迹，0 条问题
```

注意最后一行：不仅面试有评分，**协作本身也有评分**——四段轨迹零问题，这套三 Agent 系统自己的健康分是满分。

回头看这次面试本身，它还把第一系列的面试教练逻辑用多智能体重演了一遍：面试官对应「出题系统」，追问教练对应「追问策略」（自评置信度 + 关键点覆盖度决定深挖或放行），评估师对应「结构化评分报告」（维度得分 + 薄弱点 + 改进建议）。单 Agent 版本里挤在一个提示词中的三件事，现在各自独立、各自可测、各自可换模型——这就是八篇重构带给产品的真实收益。

## 从 demo 到产品，还差什么

诚实地说，demo 距离产品还差四步，都已经在前文埋好了伏笔：一是把 `scriptedLLM` 换成真实模型调用，评分环节用 JSON Schema 约束输出再解析成 struct；二是轨迹落盘成 JSON，评测可以离线重放；三是问答记录接多轮记忆，支持一场面试跨会话续跑；四是套上 TUI 或接入微信，让候选人有个能打字的地方。前两步改完，它已经是一个能在命令行里真实使用的教练。

至此「多智能体协作」系列八篇全部完结。回看这条路线：从一个 Agent 接口出发，依次解决了派活、传话、共享、调度、质量、容错，最后组装成产品——没有任何一步依赖第三方 Agent 框架。理解了原理，框架对你只是可选项。

完整工程代码已开源：[github.com/GO2AI-CN/ai-interview-coach](https://github.com/GO2AI-CN/ai-interview-coach)，multi 包全部原语与 8 个可运行 demo 都在仓库里，欢迎 Star。

如果这个系列对你有启发，欢迎点赞、在看、转发，关注「Go2AI之路」，下一系列我们聊聊 RAG 之外的新战场。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
