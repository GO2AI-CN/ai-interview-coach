// Package eval 实现《LLM 应用工程化》系列第 3 篇的自动化评测集。
package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Expect 确定性判定条件（全部可选，但至少给一个）
type Expect struct {
	Category     string   `json:"category"`      // 要求分类属于此值（| 分隔表示多选一）
	MustKeywords []string `json:"must_keywords"` // 输出必须包含的关键词
	ExpectError  bool     `json:"expect_error"`  // 期望链路拒绝该输入（如注入样本）
}

// Case 一条评测用例
type Case struct {
	ID        string   `json:"id"`
	Input     string   `json:"input"`
	Expect    Expect   `json:"expect"`
	JudgeCrit string   `json:"judge_criteria"` // 交给 LLM judge 的判定标准
}

// LoadDataset 从 JSONL 加载评测集
func LoadDataset(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cases []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024) // 防止长行被截断
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("解析用例失败: %w", err)
		}
		cases = append(cases, c)
	}
	return cases, sc.Err()
}

// DeterministicScore 确定性判分：能用规则判的绝不上模型
func DeterministicScore(output string, expect Expect) (float64, []string) {
	var failed []string
	var passed, total int

	if expect.Category != "" {
		total++
		ok := false
		for _, c := range strings.Split(expect.Category, "|") {
			if strings.Contains(output, c) {
				ok = true
				break
			}
		}
		if ok {
			passed++
		} else {
			failed = append(failed, "category 不匹配: 期望 "+expect.Category)
		}
	}
	for _, kw := range expect.MustKeywords {
		total++
		if strings.Contains(output, kw) {
			passed++
		} else {
			failed = append(failed, "缺少关键词: "+kw)
		}
	}
	if total == 0 {
		return 1.0, nil
	}
	return float64(passed) / float64(total), failed
}

// Result 单条用例的评测结果
type Result struct {
	CaseID      string   `json:"case_id"`
	DetScore    float64  `json:"det_score"`
	DetFailed   []string `json:"det_failed,omitempty"`
	JudgePass   bool     `json:"judge_pass"`
	JudgeScore  int64    `json:"judge_score"`
	JudgeReason string   `json:"judge_reason"`
	FinalPass   bool     `json:"final_pass"`
}

// JudgeFunc LLM-as-judge 抽象（生产用 llm.Client.ChatJSON 实现）；nil 时退化为纯确定性判分
type JudgeFunc func(criteria, input, output string) (pass bool, score int64, reason string, err error)

// Run 执行完整评测。evalFn 走真实业务链路（可指向任意 Prompt 版本）
func Run(cases []Case, evalFn func(input string) (string, error), judge JudgeFunc) ([]Result, error) {
	var results []Result
	for _, c := range cases {
		output, err := evalFn(c.Input)
		if err != nil {
			if c.Expect.ExpectError {
				results = append(results, Result{CaseID: c.ID, FinalPass: true})
			} else {
				results = append(results, Result{CaseID: c.ID, DetFailed: []string{"执行出错: " + err.Error()}})
			}
			continue
		}
		if c.Expect.ExpectError {
			results = append(results, Result{CaseID: c.ID, DetFailed: []string{"期望被拒绝，实际放行了"}})
			continue
		}
		detScore, detFailed := DeterministicScore(output, c.Expect)
		r := Result{CaseID: c.ID, DetScore: detScore, DetFailed: detFailed}

		if detScore == 1.0 && (c.JudgeCrit == "" || judge == nil) {
			r.FinalPass = true
		} else if judge != nil {
			pass, score, reason, jerr := judge(c.JudgeCrit, c.Input, output)
			if jerr != nil {
				r.JudgeReason = "judge 调用失败: " + jerr.Error()
			} else {
				r.JudgePass, r.JudgeScore, r.JudgeReason = pass, score, reason
				r.FinalPass = detScore == 1.0 && pass
			}
		}
		results = append(results, r)
	}
	return results, nil
}

// PassRate 整体通过率
func PassRate(results []Result) float64 {
	if len(results) == 0 {
		return 0
	}
	var passed int
	for _, r := range results {
		if r.FinalPass {
			passed++
		}
	}
	return float64(passed) / float64(len(results))
}
