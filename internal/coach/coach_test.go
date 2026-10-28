package coach

import (
	"context"
	"encoding/json"
	"testing"

	"ai-interview-coach/internal/llm"
)

// mockClient 按 schemaName 返回预定的 JSON，验证状态机全程无需真实 API。
type mockClient struct{ t *testing.T }

func (m mockClient) ChatText(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

func (m mockClient) ChatStream(_ context.Context, _, _ string, onToken func(string)) (string, error) {
	if onToken != nil {
		onToken("点评")
	}
	return "点评", nil
}

func (m mockClient) ChatJSON(_ context.Context, _, _, name, _ string, _ any, out any) error {
	m.t.Helper()
	var payload []byte
	switch name {
	case "interview_plan":
		payload = []byte(`{"questions":[
			{"dimension":"Go语言内核","question":"GMP 里为什么要有 P？","checkpoints":["减少全局锁竞争","本地队列"],"weight":50,"source":"rubric"},
			{"dimension":"并发","question":"goroutine 泄漏怎么查？","checkpoints":["pprof","阻塞分析"],"weight":50,"source":"rubric"}]}`)
	case "answer_evaluation":
		payload = []byte(`{"confidence":2,"coverage":0.5,"hit_points":["减少全局锁竞争"],"missed":["本地队列"],"suggestion":"补一下 P 的本地队列","depth_ok":false}`)
	case "interview_report":
		payload = []byte(`{"dimensions":[{"name":"Go语言内核","score":70,"comment":"原理清楚"}],"weak_points":["P 本地队列"],"suggestions":["重看调度器源码"],"overall":70}`)
	default:
		m.t.Fatalf("unexpected schema: %s", name)
	}
	return json.Unmarshal(payload, out)
}

var _ llm.Client = mockClient{}

func newTestInterview(t *testing.T) *Interview {
	t.Helper()
	it, err := NewInterview(context.Background(), mockClient{t}, "高级后端工程师", "")
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestInterviewFollowUpThenAdvance(t *testing.T) {
	it := newTestInterview(t)
	if got := len(it.State.Plan.Questions); got != 2 {
		t.Fatalf("plan questions = %d, want 2", got)
	}

	// 第 1 题：mock 评估 confidence=2 coverage=0.5 → 必须追问
	follow, prompt, err := it.Answer(context.Background(), "P 是逻辑处理器")
	if err != nil {
		t.Fatal(err)
	}
	if !follow {
		t.Fatal("expected follow-up for shallow answer")
	}
	if it.State.Cursor != 0 {
		t.Fatalf("cursor = %d, want 0 (still on Q1)", it.State.Cursor)
	}
	if it.State.Pending.FollowUps != 1 {
		t.Fatalf("followups = %d, want 1", it.State.Pending.FollowUps)
	}

	// 追问轮：checkpoints 应被替换成缺失要点（有方向的追问）
	if it.State.Pending.Question.Checkpoints[0] != "本地队列" {
		t.Fatalf("follow-up checkpoints = %v", it.State.Pending.Question.Checkpoints)
	}
	_ = prompt

	// 第 2 次回答：追问次数未满仍会追问，第 3 次才强制放行
	if _, _, err := it.Answer(context.Background(), "满了会转移到全局队列"); err != nil {
		t.Fatal(err)
	}
	if it.State.Cursor != 0 {
		t.Fatal("expected to stay on Q1 after 2nd shallow answer")
	}
	if _, _, err := it.Answer(context.Background(), "再问一次"); err != nil {
		t.Fatal(err)
	}
	if it.State.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (must advance after max follow-ups)", it.State.Cursor)
	}
	if len(it.State.Done) != 1 {
		t.Fatalf("done = %d, want 1", len(it.State.Done))
	}
}

func TestFinishBlockedUntilAllAnswered(t *testing.T) {
	it := newTestInterview(t)
	if _, err := it.Finish(context.Background()); err == nil {
		t.Fatal("Finish should fail before all questions done")
	}
}

func TestStateRoundTrip(t *testing.T) {
	it := newTestInterview(t)
	it.State.Cursor = 1
	it.State.Done = []Turn{{Question: it.State.Plan.Questions[0], Answer: "a",
		Evaluation: Evaluation{Confidence: 4, Coverage: 0.8}, FollowUps: 1}}

	path := t.TempDir() + "/state.json"
	if err := Save(it.State, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != 1 || len(got.Done) != 1 || got.Plan.Questions[0].Question != it.State.Plan.Questions[0].Question {
		t.Fatalf("state round trip mismatch: %+v", got)
	}
}
