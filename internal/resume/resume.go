// Package resume 简历解析：PDF 文本提取 + 模型摘要，
// 让面试官先读简历，再针对性出题。
package resume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	pdf "github.com/ledongthuc/pdf"
)

// ExtractText 从 PDF 提取纯文本。多页简历拼接成一个字符串。
// 纯 Go 实现，不引 CGO，交叉编译无压力。
func ExtractText(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			return "", fmt.Errorf("page %d: %w", i, err)
		}
		buf.WriteString(text)
		buf.WriteByte('\n')
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return "", errors.New("PDF 未提取到文本：可能是扫描件（图片型 PDF），需要先 OCR")
	}
	return out, nil
}

// summarySchema 约束简历摘要的输出格式。
var summarySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"skills":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"projects": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string"},
					"stack":   map[string]any{"type": "string"},
					"highlights": map[string]any{"type": "string"},
				},
				"required":             []string{"name", "stack", "highlights"},
				"additionalProperties": false,
			},
		},
		"suspect_points": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
	"required":             []string{"skills", "projects", "suspect_points"},
	"additionalProperties": false,
}

// Summarizer 抽象 LLM 摘要能力，测试时可替换。
type Summarizer interface {
	ChatJSON(ctx context.Context, system, user, schemaName, schemaDesc string, schema any, out any) error
}

// Summary 简历摘要：技能栈、项目亮点、可疑点（简历里值得深挖/验证的地方）。
type Summary struct {
	Skills        []string `json:"skills"`
	Projects      []struct {
		Name       string `json:"name"`
		Stack      string `json:"stack"`
		Highlights string `json:"highlights"`
	} `json:"projects"`
	SuspectPoints []string `json:"suspect_points"`
}

// Summarize 提取 PDF 文本并让模型输出结构化摘要。
// 摘要（而非全文）进提示词：省 token，也避免长文本稀释出题指令。
func Summarize(ctx context.Context, s Summarizer, pdfPath string) (*Summary, error) {
	text, err := ExtractText(pdfPath)
	if err != nil {
		return nil, err
	}
	const system = "你是资深 HR 与技术面试官，擅长从简历中提炼技术亮点和可疑点。"
	user := fmt.Sprintf("以下是候选人简历的提取文本，请输出结构化摘要(JSON)："+
		"skills(技能栈)、projects(项目名称/技术栈/量化亮点)、suspect_points(需要面试官验证的可疑点，"+
		"如量化数据夸张、技能与年限不匹配)。\n\n%s", truncate(text, 6000))
	var sum Summary
	if err := s.ChatJSON(ctx, system, user, "resume_summary",
		"简历结构化摘要", summarySchema, &sum); err != nil {
		return nil, err
	}
	return &sum, nil
}

// SummaryText 把摘要渲染成注入出题提示词的文本。
func (s *Summary) SummaryText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "技能栈：%s\n", strings.Join(s.Skills, "、"))
	for _, p := range s.Projects {
		fmt.Fprintf(&b, "项目「%s」(%s)：%s\n", p.Name, p.Stack, p.Highlights)
	}
	if len(s.SuspectPoints) > 0 {
		fmt.Fprintf(&b, "需要验证的点：%s\n", strings.Join(s.SuspectPoints, "；"))
	}
	return b.String()
}

// truncate 截断超长文本（留提示词余量）。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n……(截断)"
}
