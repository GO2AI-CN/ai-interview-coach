package multi

import (
	"fmt"
	"strings"
	"time"
)

// Span 是轨迹里的一段：一个 Agent 一次干活的完整记录。
// 字段选得克制——轨迹要进日志、进评测提示词，越精炼越容易被用上。
type Span struct {
	Role    string        // 谁干的
	Goal    string        // 干什么
	Input   string        // 拿到的输入摘要
	Output  string        // 产出摘要；空串视为可疑
	Latency time.Duration // 本次耗时
	Err     error         // 失败原因；nil 表示成功
}

// Trace 是一次完整协作的运行轨迹：可回放、可归因。
type Trace struct {
	Goal  string
	Spans []Span
}

// Recorder 挂在编排流程上收集 Span。零侵入设计：
// Orchestrator 不挂 Recorder 就一行不记，行为与没有轨迹时完全一致。
type Recorder struct {
	trace Trace
}

// NewRecorder 为一次协作创建记录器。
func NewRecorder(goal string) *Recorder {
	return &Recorder{trace: Trace{Goal: goal}}
}

// Record 追加一段轨迹。
func (r *Recorder) Record(s Span) { r.trace.Spans = append(r.trace.Spans, s) }

// Trace 返回目前收集到的完整轨迹。
func (r *Recorder) Trace() Trace { return r.trace }

// EvalRule 是一条评分规则：扫描整条轨迹，返回发现的问题（空=通过）。
// 规则是纯函数，可单测；以后接入评分模型时，模型输出也落到这个签名里。
type EvalRule func(Trace) []string

// Report 是轨迹评测结果。
type Report struct {
	Score    int      // 百分制，100 起扣
	Problems []string // 每条问题都带 span 序号和角色，可归因
	Spans    int      // 轨迹总段数
}

// Evaluate 用一组规则给轨迹打分。统一 10 分/条，简单可解释，
// 比一上来就加权平均更抗争议——先把硬伤找出来，再谈细化权重。
func Evaluate(t Trace, rules ...EvalRule) Report {
	rep := Report{Score: 100, Spans: len(t.Spans)}
	for _, rule := range rules {
		for _, p := range rule(t) {
			rep.Problems = append(rep.Problems, p)
			rep.Score -= 10
		}
	}
	if rep.Score < 0 {
		rep.Score = 0
	}
	return rep
}

// RuleAllSucceeded：任何一棒报错都是硬伤。
func RuleAllSucceeded(t Trace) []string {
	var out []string
	for i, s := range t.Spans {
		if s.Err != nil {
			out = append(out, fmt.Sprintf("span %d [%s] 失败: %v", i, s.Role, s.Err))
		}
	}
	return out
}

// RuleNoEmptyOutput：产出为空的 Agent 在划水，要么提示词有问题要么模型偷懒。
func RuleNoEmptyOutput(t Trace) []string {
	var out []string
	for i, s := range t.Spans {
		if s.Err == nil && strings.TrimSpace(s.Output) == "" {
			out = append(out, fmt.Sprintf("span %d [%s] 产出为空", i, s.Role))
		}
	}
	return out
}

// RuleLatencyUnder：单棒耗时超限要报出来，通常是提示词膨胀或模型抖动。
func RuleLatencyUnder(limit time.Duration) EvalRule {
	return func(t Trace) []string {
		var out []string
		for i, s := range t.Spans {
			if s.Latency > limit {
				out = append(out, fmt.Sprintf("span %d [%s] 耗时 %v 超过 %v", i, s.Role, s.Latency, limit))
			}
		}
		return out
	}
}
