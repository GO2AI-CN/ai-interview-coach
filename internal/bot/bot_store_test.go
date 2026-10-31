package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recInterview 在 fakeInterview 之上补 Recorder，模拟真实 coach 适配器。
type recInterview struct {
	fakeInterview
	recorded bool
}

func (r *recInterview) Record() (string, int, []Turn) {
	r.recorded = true
	return "Go 后端工程师", 80, []Turn{
		{Dimension: "并发", Question: "Q1", Answer: "A1", Confidence: 4, Coverage: 0.8, FollowUps: 1},
		{Dimension: "切片", Question: "Q2", Answer: "A2", Confidence: 5, Coverage: 1, FollowUps: 0},
	}
}

// saveCall 记录一次 Save 调用的入参，测试断言用。
type saveCall struct {
	key, position, report string
	overall               int
	turns                 []Turn
}

// fakeStore 内存版 Store，不碰真实数据库。
type fakeStore struct {
	calls  []saveCall
	recent []HistoryItem
	report string
	err    error // 非 nil 时 Save/Recent/Report 都返回它
}

func (f *fakeStore) Save(_ context.Context, key, position string, overall int, reportMD string, turns []Turn) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.calls = append(f.calls, saveCall{key, position, reportMD, overall, turns})
	return int64(len(f.calls)), nil
}

func (f *fakeStore) Recent(_ context.Context, _ string, _ int) ([]HistoryItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.recent, nil
}

func (f *fakeStore) Report(_ context.Context, _ string, _ int64) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.report, nil
}

// newStoreBot 构造一个注入 fakeStore、面试由 Recorder fake 承担的机器人。
func newStoreBot(t *testing.T, st Store) (*Bot, *recInterview) {
	t.Helper()
	ri := &recInterview{}
	b := NewBot(&fakeSender{}, func(_ context.Context, _ string) (Interview, error) {
		return ri, nil
	}, 0, WithStore(st))
	return b, ri
}

func runInterview(t *testing.T, b *Bot) string {
	t.Helper()
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "k", "面试 Go 后端工程师"); err != nil {
		t.Fatal(err)
	}
	// 第 1 次作答触发追问，第 2 次作答面完（fake 的脚本）
	if _, err := b.HandleText(ctx, "k", "channel 用于通信"); err != nil {
		t.Fatal(err)
	}
	got, err := b.HandleText(ctx, "k", "补充：mutex 保护临界区")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "面试结束") {
		t.Fatalf("fake 2 题应已面完, got=%q", got)
	}
	return got
}

func TestSaveOnFinish(t *testing.T) {
	st := &fakeStore{}
	b, ri := newStoreBot(t, st)

	got := runInterview(t, b)

	if !ri.recorded {
		t.Fatal("应调用 Record 提取落库数据")
	}
	if len(st.calls) != 1 {
		t.Fatalf("应落库 1 场, got=%d", len(st.calls))
	}
	c := st.calls[0]
	if c.key != "k" || c.position != "Go 后端工程师" || c.overall != 80 || c.report != "# 评分报告\n综合得分：80/100" {
		t.Fatalf("落库字段不对: %+v", c)
	}
	if len(c.turns) != 2 || c.turns[0].FollowUps != 1 {
		t.Fatalf("落库明细不对: %+v", c.turns)
	}
	if !strings.Contains(got, "已存入历史") {
		t.Fatalf("回复应提示已入库, got=%q", got)
	}
}

func TestSaveOnEarlyFinish(t *testing.T) {
	st := &fakeStore{}
	b, _ := newStoreBot(t, st)
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "k", "面试 SRE"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.HandleText(ctx, "k", "结束"); err != nil {
		t.Fatal(err)
	}
	if len(st.calls) != 1 {
		t.Fatalf("提前结束也应落库, got=%d", len(st.calls))
	}
}

func TestSaveFailureKeepsReport(t *testing.T) {
	st := &fakeStore{err: errors.New("disk full")}
	b, _ := newStoreBot(t, st)

	got := runInterview(t, b)

	if !strings.Contains(got, "评分报告") {
		t.Fatalf("报告必须在, got=%q", got)
	}
	if !strings.Contains(got, "落库失败") {
		t.Fatalf("应提示落库失败而不是吞掉, got=%q", got)
	}
}

func TestNoRecorderNoSave(t *testing.T) {
	st := &fakeStore{}
	// 用普通 fakeInterview（不实现 Recorder）+ store
	b := NewBot(&fakeSender{}, func(_ context.Context, _ string) (Interview, error) {
		return &fakeInterview{}, nil
	}, 0, WithStore(st))

	runInterview(t, b)

	if len(st.calls) != 0 {
		t.Fatalf("不实现 Recorder 就不应落库, got=%d", len(st.calls))
	}
}

func TestHistoryListAndPick(t *testing.T) {
	st := &fakeStore{
		recent: []HistoryItem{{ID: 2, Position: "SRE", Overall: 91}, {ID: 1, Position: "Go 后端", Overall: 80}},
		report: "# 报告\n内容",
	}
	b, _ := newStoreBot(t, st)
	ctx := context.Background()

	got, err := b.HandleText(ctx, "k", "历史")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "1. SRE") || !strings.Contains(got, "2. Go 后端") {
		t.Fatalf("列表顺序不对（新的在前）: %q", got)
	}

	got, err = b.HandleText(ctx, "k", "历史 1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# 报告") || !strings.Contains(got, "SRE") {
		t.Fatalf("应按序号取报告, got=%q", got)
	}
}

func TestHistoryPickWithoutList(t *testing.T) {
	st := &fakeStore{report: "x"}
	b, _ := newStoreBot(t, st)
	got, err := b.HandleText(context.Background(), "k", "历史 1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "先回复「历史」") {
		t.Fatalf("未取列表时应引导, got=%q", got)
	}
}

func TestHistoryNotEnabled(t *testing.T) {
	b, _, _ := newTestBot(t) // 没注入 store
	got, err := b.HandleText(context.Background(), "k", "历史")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "未开启") {
		t.Fatalf("应提示功能未开启, got=%q", got)
	}
}

func TestHistoryEmpty(t *testing.T) {
	st := &fakeStore{} // recent 为空
	b, _ := newStoreBot(t, st)
	got, err := b.HandleText(context.Background(), "k", "历史")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "还没有面试记录") {
		t.Fatalf("空历史提示不对, got=%q", got)
	}
}
