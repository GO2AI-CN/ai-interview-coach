// Package report 把结构化评分报告渲染成 Markdown。
package report

import (
	"fmt"
	"strings"

	"ai-interview-coach/internal/coach"
)

// Markdown 渲染评分报告，输出可直接贴进笔记软件的 Markdown 文本。
func Markdown(rep *coach.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 模拟面试评分报告\n\n")
	fmt.Fprintf(&b, "- 目标岗位：%s\n", rep.Position)
	fmt.Fprintf(&b, "- 综合得分：**%d / 100**\n\n", rep.Overall)

	b.WriteString("## 各维度得分\n\n")
	b.WriteString("| 能力维度 | 得分 | 评语 |\n|---|---|---|\n")
	for _, d := range rep.Dimensions {
		fmt.Fprintf(&b, "| %s | %d | %s |\n", d.Name, d.Score, d.Comment)
	}
	b.WriteString("\n")

	b.WriteString("## 薄弱点\n\n")
	for _, w := range rep.WeakPoints {
		fmt.Fprintf(&b, "- %s\n", w)
	}
	b.WriteString("\n## 改进建议\n\n")
	for _, s := range rep.Suggestions {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	return b.String()
}
