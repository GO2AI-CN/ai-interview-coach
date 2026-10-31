package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"ai-interview-coach/internal/bot"
)

func openTemp(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleTurns() []bot.Turn {
	return []bot.Turn{
		{Dimension: "并发", Question: "Q1", Answer: "A1", Confidence: 4, Coverage: 0.75, FollowUps: 2},
		{Dimension: "切片", Question: "Q2", Answer: "A2", Confidence: 5, Coverage: 1, FollowUps: 0},
	}
}

func TestSaveRecentReport(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	id1, err := s.Save(ctx, "k1", "Go 后端工程师", 72, "# 报告一", sampleTurns())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "k1", "SRE", 91, "# 报告二", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "k2", "架构师", 60, "# 别人的报告", nil); err != nil {
		t.Fatal(err)
	}

	// Recent：按会话隔离，新的在前
	items, err := s.Recent(ctx, "k1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Position != "SRE" || items[1].Position != "Go 后端工程师" {
		t.Fatalf("Recent 不对: %+v", items)
	}
	if items[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt 应为有效时间")
	}

	// limit 生效
	one, err := s.Recent(ctx, "k1", 1)
	if err != nil || len(one) != 1 || one[0].Position != "SRE" {
		t.Fatalf("limit=1 不对: %+v, err=%v", one, err)
	}

	// Report：按 ID 取回，且不能跨会话猜 ID
	rep, err := s.Report(ctx, "k1", id1)
	if err != nil || rep != "# 报告一" {
		t.Fatalf("Report 不对: %q, err=%v", rep, err)
	}
	if _, err := s.Report(ctx, "k1", 3); err == nil {
		t.Fatal("跨会话取报告必须报错")
	}
	if _, err := s.Report(ctx, "k1", 999); err == nil {
		t.Fatal("不存在的 ID 必须报错")
	}
}

func TestReopenKeepsData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coach.db")
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Save(ctx, "k", "Go", 80, "# 持久化", sampleTurns())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// 重新打开：数据还在（证明落的是文件，不是内存）
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rep, err := s2.Report(ctx, "k", id)
	if err != nil || rep != "# 持久化" {
		t.Fatalf("重开后报告丢失: %q, err=%v", rep, err)
	}
	items, err := s2.Recent(ctx, "k", 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("重开后列表丢失: %+v, err=%v", items, err)
	}
}

// TestConcurrentSave 验证「单写者」策略：SQLite 同时只允许一个写入，
// MaxOpenConns(1) 把并发写串行化，调用方不用自己做重试。
func TestConcurrentSave(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Save(ctx, "k", "Go", 80, "# 并发", sampleTurns())
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写失败（单连接串行化应挡住 busy）: %v", err)
		}
	}
	items, err := s.Recent(ctx, "k", n)
	if err != nil || len(items) != n {
		t.Fatalf("应存进 %d 场: len=%d, err=%v", n, len(items), err)
	}
}
