package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ai-interview-coach/internal/bot"
	"ai-interview-coach/internal/store"
)

// fakeLLM 用函数实现 LLM 接口，每轮可以看到完整消息历史并做断言。
type fakeLLM func(messages []Message, tools []Tool) (*Message, error)

func (f fakeLLM) Chat(_ context.Context, messages []Message, tools []Tool) (*Message, error) {
	return f(messages, tools)
}

// 正常路径：第一轮要工具，第二轮读到结果后给纯文本答复。
func TestLoopToolCallThenAnswer(t *testing.T) {
	var execCalls []string
	exec := func(_ context.Context, name string, args json.RawMessage) (string, error) {
		execCalls = append(execCalls, name+":"+string(args))
		return "工具结果: 42", nil
	}
	llm := fakeLLM(func(messages []Message, tools []Tool) (*Message, error) {
		switch {
		case len(messages) == 2: // 只有 system+user
			if len(tools) != 1 || tools[0].Name != "query" {
				t.Errorf("tools 未传入: %+v", tools)
			}
			return &Message{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "c1", Name: "query", Arguments: json.RawMessage(`{"q":"x"}`)}}}, nil
		default:
			last := messages[len(messages)-1]
			if last.Role != "tool" || last.ToolCallID != "c1" || last.Text != "工具结果: 42" {
				t.Errorf("工具结果没喂回来: %+v", last)
			}
			return &Message{Role: "assistant", Text: "最终答复"}, nil
		}
	})

	answer, err := RunLoop(context.Background(), llm, exec,
		"sys", "question", []Tool{{Name: "query"}}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "最终答复" {
		t.Errorf("answer = %q", answer)
	}
	if len(execCalls) != 1 || execCalls[0] != `query:{"q":"x"}` {
		t.Errorf("execCalls = %v", execCalls)
	}
}

// 关键契约：工具失败不中止循环，错误文本作为 tool 消息喂回模型。
func TestLoopToolErrorFedBack(t *testing.T) {
	exec := func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		return "", errors.New("除数不能为 0")
	}
	var sawError bool
	llm := fakeLLM(func(messages []Message, _ []Tool) (*Message, error) {
		for _, m := range messages {
			if m.Role == "tool" && strings.Contains(m.Text, "工具调用失败：除数不能为 0") {
				sawError = true
			}
		}
		if !sawError {
			return &Message{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "c1", Name: "calc", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		return &Message{Role: "assistant", Text: "我重试过了，换个问法"}, nil
	})

	answer, err := RunLoop(context.Background(), llm, exec, "sys", "q", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !sawError || answer != "我重试过了，换个问法" {
		t.Errorf("错误未被喂回模型: sawError=%v answer=%q", sawError, answer)
	}
}

// 防护：模型无限要工具时必须撞 maxSteps 上限，而不是死循环。
func TestLoopMaxStepsGuard(t *testing.T) {
	execCount := 0
	exec := func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		execCount++
		return "ok", nil
	}
	llm := fakeLLM(func(_ []Message, _ []Tool) (*Message, error) {
		return &Message{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "loop", Name: "again"}}}, nil
	})

	_, err := RunLoop(context.Background(), llm, exec, "sys", "q", nil, 3)
	if err == nil || !strings.Contains(err.Error(), "最大轮数") {
		t.Fatalf("want maxSteps error, got %v", err)
	}
	if execCount != 3 {
		t.Errorf("execCount = %d, want 3", execCount)
	}
}

// TestEndToEndWithCoachstore 用真编译的 coachstore server + scriptedLLM
// 走完整链路：握手 → tools/list → 循环（list_sessions → get_report → 总结）。
func TestEndToEndWithCoachstore(t *testing.T) {
	// 造夹具库：与 coachstore 的测试同款套路
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

// 通知不产生响应、错 id 要对账——已在 e2e 中间接覆盖；
// 这里单独验证 server 退出后 call 返回错误而不是挂死。
func TestCallReturnsErrorWhenServerDies(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "exits.exe")
	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build trivial: %v\n%s", err, out)
	}

	ctx := context.Background()
	// 直连一个空进程：initialize 就该失败
	_, err := NewClient(ctx, bin)
	if err == nil {
		t.Fatal("server 立即退出时 NewClient 应报错")
	}
}
