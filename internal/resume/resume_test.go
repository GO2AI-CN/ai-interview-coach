package resume

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type mockSummarizer struct{ gotUser string }

func (m *mockSummarizer) ChatJSON(_ context.Context, _, user, name, _ string, _ any, out any) error {
	if name != "resume_summary" {
		return nil
	}
	m.gotUser = user
	payload := []byte(`{"skills":["Go","Redis"],"projects":[{"name":"订单网关","stack":"Go/K8s","highlights":"百万级 QPS"}],"suspect_points":["QPS 数据待验证"]}`)
	return json.Unmarshal(payload, out)
}

func TestSummarizeEndToEnd(t *testing.T) {
	ms := &mockSummarizer{}
	sum, err := Summarize(context.Background(), ms, "testdata/resume.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Skills) != 2 || sum.Projects[0].Name != "订单网关" {
		t.Fatalf("unexpected summary: %+v", sum)
	}
	// 提示词里应包含 PDF 提取出的文本（说明简历真的被"读"了）
	if !strings.Contains(ms.gotUser, "Zhang San") {
		t.Fatal("resume text not injected into prompt")
	}
	// 摘要文本要能注入出题提示词
	if !strings.Contains(sum.SummaryText(), "订单网关") || !strings.Contains(sum.SummaryText(), "QPS 数据待验证") {
		t.Fatalf("SummaryText missing fields: %s", sum.SummaryText())
	}
}

func TestExtractTextEmptyFile(t *testing.T) {
	if _, err := ExtractText("testdata/none.pdf"); !os.IsNotExist(err) && err == nil {
		t.Fatal("expected error for missing file")
	}
}
