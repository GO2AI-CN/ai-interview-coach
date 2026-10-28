package report

import (
	"strings"
	"testing"

	"ai-interview-coach/internal/coach"
)

func TestMarkdown(t *testing.T) {
	rep := &coach.Report{
		Position:   "高级后端工程师",
		Overall:    72,
		Dimensions: []coach.DimensionScore{{Name: "Go语言内核", Score: 70, Comment: "原理清楚"}},
		WeakPoints: []string{"P 本地队列"},
		Suggestions: []string{"重看调度器源码"},
	}
	md := Markdown(rep)
	for _, want := range []string{"72 / 100", "Go语言内核", "| 70 |", "P 本地队列", "重看调度器源码"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}
