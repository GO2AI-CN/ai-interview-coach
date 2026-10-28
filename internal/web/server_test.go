package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-interview-coach/internal/coach"
)

// streamMock 覆盖全部 LLM 调用：ChatJSON 返回评估，ChatStream 按 token 回调。
type streamMock struct{ evalJSON string }

func (m streamMock) ChatJSON(_ context.Context, _, _, name, _ string, _ any, out any) error {
	if name == "answer_evaluation" {
		return json.Unmarshal([]byte(m.evalJSON), out)
	}
	return nil
}

func (m streamMock) ChatText(_ context.Context, _, _ string) (string, error) { return "", nil }

func (streamMock) ChatStream(_ context.Context, _, user string, onToken func(string)) (string, error) {
	for _, tok := range []string{"覆", "盖", "率", "偏", "低"} {
		onToken(tok)
	}
	_ = user
	return "覆盖率偏低", nil
}

func TestAnswerSSEFormat(t *testing.T) {
	// 构造已推进到第 1 题的面试
	st := &coach.State{
		Position: "高级后端工程师",
		Plan: coach.Plan{Questions: []coach.Question{
			{Dimension: "Go", Question: "GMP 是什么", Checkpoints: []string{"调度"}, Weight: 100, Source: "rubric"},
		}},
	}
	srv := NewServer(streamMock{evalJSON: `{"confidence":2,"coverage":0.5,"hit_points":["调度"],"missed":["本地队列"],"suggestion":"补一下","depth_ok":false}`})
	srv.SetSessionForTest("s1", coach.ResumeInterview(st, streamMock{evalJSON: `{"confidence":2,"coverage":0.5,"hit_points":["调度"],"missed":["本地队列"],"suggestion":"补一下","depth_ok":false}`}))

	req := httptest.NewRequest("GET", "/api/answer?session=s1&a="+urlQueryEscape("调度器"), nil)
	rec := httptest.NewRecorder()
	srv.handleAnswer(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	// SSE 每个事件以空行分隔，且必须包含 token/result/done 三类事件
	for _, want := range []string{"event: token", "event: result", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE stream missing %q:\n%s", want, body)
		}
	}
	// token 事件按字推送
	if !strings.Contains(body, `"t":"覆"`) || !strings.Contains(body, `"t":"低"`) {
		t.Errorf("token events not per-character:\n%s", body)
	}
	// 结构化事件可解析
	sc := bufio.NewScanner(strings.NewReader(body))
	var sawDone bool
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: done") {
			sawDone = true
		}
		if strings.HasPrefix(line, "data: ") && sawDone {
			var v map[string]bool
			if err := json.Unmarshal([]byte(line[6:]), &v); err != nil {
				t.Errorf("done payload not json: %v", err)
			}
		}
	}
}

func urlQueryEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", "&", "%26", "=", "%3D", "+", "%2B", "#", "%23")
	return r.Replace(s)
}
