package coach

import (
	"context"
	"encoding/json"

	"ai-interview-coach/internal/llm"
)

// Interview 是面试状态机：按计划推进、评估、决定追问还是放行。
type Interview struct {
	State  *State
	client llm.Client
}

// NewInterview 生成面试计划，开启一场新面试。resume 可为空（不传简历）。
func NewInterview(ctx context.Context, client llm.Client, position, resume string) (*Interview, error) {
	sys, user := planPrompt(position, resume)
	var plan Plan
	if err := client.ChatJSON(ctx, sys, user, "interview_plan",
		"面试计划：能力维度题目清单与考察要点", PlanSchema, &plan); err != nil {
		return nil, err
	}
	return &Interview{State: &State{Position: position, Plan: plan, Resume: resume}, client: client}, nil
}

// ResumeInterview 从持久化状态恢复一场面试（多轮记忆）。
func ResumeInterview(st *State, client llm.Client) *Interview {
	return &Interview{State: st, client: client}
}

// CurrentQuestion 返回当前应回答的题目。
func (it *Interview) CurrentQuestion() *Question {
	if it.State.Cursor >= len(it.State.Plan.Questions) {
		return nil // 全部问完
	}
	return &it.State.Plan.Questions[it.State.Cursor]
}

// Answer 提交一轮回答：评估 → 决定追问还是进入下一题。
// 返回 (是否需要追问, 追问/评估提示, error)。
func (it *Interview) Answer(ctx context.Context, answer string) (bool, string, error) {
	q := it.CurrentQuestion()
	if q == nil {
		return false, "", errFinished
	}
	if it.State.Pending == nil {
		it.State.Pending = &Turn{Question: *q}
	}
	it.State.Pending.Answer = answer

	sys, user := evalPrompt(it.State.Pending.Question, answer)
	var ev Evaluation
	if err := it.client.ChatJSON(ctx, sys, user, "answer_evaluation",
		"对面试回答的结构化评估", EvalSchema, &ev); err != nil {
		return false, "", err
	}
	it.State.Pending.Evaluation = ev
	it.State.Pending.FollowUps++

	// 两把尺子决定追问 or 下一题
	if ShouldFollowUp(ev, it.State.Pending.FollowUps-1) && len(ev.Missed) > 0 {
		// 追问有方向：把缺失要点换成本轮的考察标准
		it.State.Pending.Question.Question = FollowUpPrompt(ev.Missed)
		it.State.Pending.Question.Checkpoints = ev.Missed
		return true, it.State.Pending.Question.Question, nil
	}
	it.finishQuestion()
	return false, "本题评估完成，进入下一题。", nil
}

// finishQuestion 把当前题归档进 Done，推进游标。
func (it *Interview) finishQuestion() {
	it.State.Done = append(it.State.Done, *it.State.Pending)
	it.State.Pending = nil
	it.State.Cursor++
}

// Finish 全部问完后生成结构化评分报告并渲染成 Markdown。
func (it *Interview) Finish(ctx context.Context) (*Report, error) {
	if q := it.CurrentQuestion(); q != nil {
		return nil, errNotFinished
	}
	sys, user := reportPrompt(it.State.Position, it.State.Done)
	var rep Report
	if err := it.client.ChatJSON(ctx, sys, user, "interview_report",
		"结构化面试评分报告", ReportSchema, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// turnsJSON 把问答记录序列化进报告提示词，作为多轮记忆的上下文。
func turnsJSON(turns []Turn) string {
	b, err := json.Marshal(turns)
	if err != nil {
		return "[]"
	}
	return string(b)
}
