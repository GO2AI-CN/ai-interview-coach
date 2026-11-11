package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ai-interview-coach/internal/bot"
	"ai-interview-coach/internal/store"
)

// TestEndToEndWithCoachstore 复用手写版 e2e 的夹具套路：
// 真编译的 coachstore server（手写协议栈）+ SDK client（官方协议栈）+
// scriptedLLM，验证两条实现能互通——这正是系列二第 5 篇与第 8 篇的合流。
func TestEndToEndWithCoachstore(t *testing.T) {
	// 造夹具库：与 coachclient 的测试同款套路
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "coach.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(context.Background(), "feishu:g-1", "Go 后端工程师", 88,
		"# 报告 · Go 后端工程师\n\n综合 88 分，并发维度需加强。", []bot.Turn{
			{Dimension: "并发", Question: "GMP？", Answer: "……", Confidence: 3, Coverage: 0.5, FollowUps: 2},
		}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "coachstore.exe")
	if out, err := exec.Command("go", "build", "-o", bin, "../coachstore").CombinedOutput(); err != nil {
		t.Fatalf("build coachstore: %v\n%s", err, out)
	}

	t.Setenv("COACH_DB_PATH", dbPath) // NewClient 会把环境透传给 server 子进程
	ctx := context.Background()
	client, err := NewClient(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	// SDK 协商出的协议版本应不落后于手写版钉住的 2025-06-18
	if v := client.session.InitializeResult().ProtocolVersion; v == "" {
		t.Fatal("InitializeResult 缺 protocolVersion")
	}

	answer, err := RunLoop(ctx, &scriptedLLM{},
		func(ctx context.Context, name string, args json.RawMessage) (string, error) {
			return client.CallTool(ctx, name, args)
		},
		systemPrompt, "我最近的面试情况怎么样？", tools, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "综合 88 分") {
		t.Errorf("最终答复应包含报告内容: %q", answer)
	}
}

// 防护：server 立即退出时 Connect 应报错而不是挂死（对手写版
// TestCallReturnsErrorWhenServerDies 的 SDK 版复刻）。
func TestConnectReturnsErrorWhenServerDies(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "exits.exe")
	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build trivial: %v\n%s", err, out)
	}

	ctx := context.Background()
	_, err := NewClient(ctx, bin)
	if err == nil {
		t.Fatal("server 立即退出时 NewClient 应报错")
	}
}
