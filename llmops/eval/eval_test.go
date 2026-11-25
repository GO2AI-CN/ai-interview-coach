package eval

import (
	"strings"
	"testing"
)

// stubEvalFn 离线桩：模拟业务链路对不同输入的输出
func stubEvalFn(input string) (string, error) {
	switch {
	case strings.Contains(input, "Ignore all previous"):
		return "", errRejected
	case strings.Contains(input, "panic"):
		return `{"sentiment":"negative","category":"refund","summary":"客户要求退款","urgent":true,"tags":["退款","投诉","v3"]}`, nil
	default:
		return `{"sentiment":"neutral","category":"consult","summary":"咨询批量导入支持","urgent":false,"tags":["咨询","API"]}`, nil
	}
}

type rejectError struct{}

func (rejectError) Error() string { return "输入包含可疑指令内容" }

var errRejected = rejectError{}

// TestGoldenDataset 黄金数据集回归（第 3 篇 CI 门禁的离线形态）。
// 生产注入 LLM judge（用 internal/llm.Client.ChatJSON 实现 JudgeFunc），
// 并在 CI 设置 OPENAI_API_KEY。
func TestGoldenDataset(t *testing.T) {
	cases, err := LoadDataset("../evals/dataset.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	results, err := Run(cases, stubEvalFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		t.Logf("case=%s pass=%v det=%.2f failed=%v", r.CaseID, r.FinalPass, r.DetScore, r.DetFailed)
	}
	if got := PassRate(results); got < 0.99 {
		t.Errorf("通过率 %.0f%% 低于门禁 99%%", got*100)
	}
}
