package guard

import (
	"fmt"
	"regexp"
)

// DetectRule 一条注入特征规则
type DetectRule struct {
	Name    string
	Pattern *regexp.Regexp
	Level   string // "high" | "medium"
}

// InjectionDetector 基于模式的注入检测（第 7 篇第 2 层防线）
// 生产可叠加模型分类器；规则判定的优势是零成本、可复现
type InjectionDetector struct {
	patterns []DetectRule
}

func NewInjectionDetector() *InjectionDetector {
	return &InjectionDetector{patterns: []DetectRule{
		{"指令覆盖", regexp.MustCompile(`(?i)ignore (all |any )?(previous|prior|above) (instructions?|prompts?)`), "high"},
		{"指令覆盖中文", regexp.MustCompile(`忽略(你)?(之前|以上|所有)(的)?(指令|提示词|要求)`), "high"},
		{"提示词窃取", regexp.MustCompile(`(?i)(print|show|repeat|reveal|输出|告诉我).{0,12}(system )?prompt|系统提示词`), "high"},
		{"角色扮演逃逸", regexp.MustCompile(`(?i)DAN mode|jailbreak|越狱模式`), "medium"},
		{"伪系统标记", regexp.MustCompile(`<\s*/?\s*system\s*>`), "high"},
	}}
}

// DetectResult 检测命中情况
type DetectResult struct {
	Hit     bool
	Matches []string
}

// Scan 扫描文本；调用侧决定拦截还是打标放行
func (d *InjectionDetector) Scan(text string) DetectResult {
	var res DetectResult
	for _, r := range d.patterns {
		if m := r.Pattern.FindString(text); m != "" {
			res.Hit = true
			res.Matches = append(res.Matches, fmt.Sprintf("%s(%s)", r.Name, r.Level))
		}
	}
	return res
}

// WrapData 把外部内容显式标记为数据（第 7 篇第 1 层防线：数据边界声明）
func WrapData(tag, content string) string {
	return fmt.Sprintf("<%s>\n%s\n</%s>", tag, content, tag)
}
