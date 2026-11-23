// 多智能体系列第 8 篇（收官）：三 Agent 模拟面试教练。
// 组装全系列原语：Guard（容错）+ Recorder（轨迹）+ Handoff（传话）+ 规则评测。
// 不接真实模型也能跑：LLM 是接口，scriptedLLM 按提示词类型返回脚本答案，
// 生产环境把这个实现换成 openai-go 客户端即可，其余代码一行不改。
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ai-interview-coach/multi"
)

// LLM 是 Agent 背后的大脑抽象。
type LLM interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// scriptedLLM 按系统提示词的"类型关键词"返回脚本答案。
// 演示了三个角色的系统提示词长什么样；真实实现里这里是 HTTP 调用。
type scriptedLLM struct{}

func (scriptedLLM) Complete(_ context.Context, system, user string) (string, error) {
	switch {
	case strings.Contains(system, "出题"):
		// 面试官：按岗位能力模型（rubric）生成面试计划
		return "1. 介绍一下 channel 的底层实现\n2. context 取消是如何传播的", nil
	case strings.Contains(system, "追问"):
		// 追问教练：答案太浅就深挖，答案合格就放行至下一题。
		// 脚本版按关键词判断；真实实现里这里是大模型的结构化判断。
		if strings.Contains(user, "完整作答") {
			return "DECISION: next", nil
		}
		return "DECISION: follow_up\n追问：能具体说说 hchan 结构体里发送队列和接收队列的关系吗？", nil
	case strings.Contains(system, "评分"):
		// 评估师：按维度打分并给出改进建议（生产环境用 JSON Schema 约束）
		return "沟通能力 85 | 专业深度 60（channel 细节缺失，被追问后仍答不出）| 工程经验 70\n改进建议：补 channel 底层结构、GMP 调度两章，再模拟一轮", nil
	}
	return "", fmt.Errorf("未知提示词类型: %.20q", system)
}

// coachAgent 把 Task 包装成 system+user 消息交给 LLM，是多 Agent 共用的骨架。
type coachAgent struct {
	name   string
	system string
	llm    LLM
}

func (c coachAgent) Name() string { return c.name }

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

// guard 给每个 Agent 套统一保险丝：超时、重试、熔断、降级。
func guard(a multi.Agent) multi.Agent {
	return multi.NewGuard(a,
		multi.WithTimeout(5*time.Second),
		multi.WithRetry(2),
		multi.WithCircuitBreaker(3, 10*time.Second),
		multi.WithFallback(func(t multi.Task) (multi.Result, error) {
			return multi.Result{Summary: "[降级] 本环节使用缓存题库"}, nil
		}),
	)
}

func main() {
	ctx := context.Background()
	llm := scriptedLLM{}
	rec := multi.NewRecorder("Go 后端模拟面试")

	interviewer := guard(coachAgent{"interviewer", "你是面试官。根据岗位能力模型（rubric）出题，一次输出全部面试题目。", llm})
	prober := guard(coachAgent{"prober", "你是追问教练。评估候选人答案的深度：答得浅就追问深挖，答到位就放行至下一题。首行输出 DECISION: follow_up 或 next。", llm})
	grader := guard(coachAgent{"grader", "你是评估师。面试结束后按能力维度评分（0-100），指出薄弱点并给出改进建议。", llm})

	// 第一步：面试官按岗位出题（产出作为 Handoff 传给追问与评分环节）
	start := time.Now()
	planRes, err := interviewer.Run(ctx, multi.Task{Goal: "岗位：Go 后端工程师（3 年经验）"})
	must(rec, "interviewer", start, planRes, err)
	questions := strings.Split(strings.TrimSpace(planRes.Output), "\n")

	// 第二步：逐题问答，追问教练动态决定是否深挖（动态循环，不走静态 plan）
	var transcript strings.Builder
	answers := map[string]string{ // 脚本候选人：第 1 题答得浅，会触发追问
		"1": "channel 就是用来在 goroutine 之间传数据的",
		"2": "context 取消会从父节点往子节点传播，子节点监听 Done 通道后退出……（完整作答）",
	}
	for _, q := range questions {
		qid := strings.SplitN(q, ".", 2)[0]
		fmt.Println("【面试官】", strings.TrimSpace(q))
		answer := answers[qid]
		fmt.Println("【候选人】", answer)

		start = time.Now()
		dec, err := prober.Run(ctx, multi.Task{
			Goal:   "判断是否需要追问",
			Inputs: map[string]string{"问题": q, "答案": answer},
		})
		must(rec, "prober", start, dec, err)

		if followUp, ok := strings.CutPrefix(dec.Output, "DECISION: follow_up\n"); ok {
			followUpLine := strings.SplitN(followUp, "\n", 2)[0]
			fmt.Println("【追问教练】", followUpLine)
			fmt.Println("【候选人】（沉默……没答上来）")
			fmt.Fprintf(&transcript, "Q:%s A:%s 追问未答出\n", q, answer)
		} else {
			fmt.Fprintf(&transcript, "Q:%s A:%s\n", q, answer)
		}
	}

	// 第三步：评估师基于完整问答记录输出结构化评分报告
	start = time.Now()
	report, err := grader.Run(ctx, multi.Task{
		Goal:   "根据以下问答记录输出评分报告",
		Inputs: map[string]string{"问答记录": transcript.String()},
	})
	must(rec, "grader", start, report, err)
	fmt.Println("\n==== 评分报告 ====")
	fmt.Println(report.Output)

	// 收官彩蛋：用第 6 篇的规则评测这次协作本身的轨迹
	rep := multi.Evaluate(rec.Trace(),
		multi.RuleAllSucceeded, multi.RuleNoEmptyOutput)
	fmt.Printf("\n协作轨迹自评：%d 分，%d 段轨迹，%d 条问题\n",
		rep.Score, rep.Spans, len(rep.Problems))
}

// must 记录一段轨迹并在出错时终止 demo。
func must(rec *multi.Recorder, role string, start time.Time, r multi.Result, err error) {
	rec.Record(multi.Span{
		Role: role, Output: r.Summary,
		Latency: time.Since(start), Err: err,
	})
	if err != nil {
		panic(err)
	}
}
