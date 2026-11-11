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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixtureDB 造一份与手写版测试同款的夹具历史库。
func fixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "coach.db")
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
	return dbPath
}

// TestToolsAgainstFixture 用 SDK 自带的内存 transport 起 server，
// SDK client 直连，三个工具全走一遍：正常路径 + 业务错误（isError 引路）路径。
func TestToolsAgainstFixture(t *testing.T) {
	db, err := openDB(fixtureDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	serverT, clientT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := buildServer(db).Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}

	cli := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools.Tools))
	}

	// 业务错误走 isError 而非协议错误：模型读到错误文本会换参数重试
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_report",
		Arguments: map[string]any{"id": 999},
	})
	if err != nil {
		t.Fatal(err) // 协议层不该报错
	}
	if !res.IsError {
		t.Fatal("不存在的 ID 应返回 isError 结果")
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "list_sessions") {
		t.Errorf("错误文案应引路 list_sessions: %q", text)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_sessions",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "综合 88 分") {
		t.Errorf("list_sessions 应包含夹具场次: %q", text)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_session",
		Arguments: map[string]any{"id": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "置信度3") {
		t.Errorf("get_session 应包含逐题指标: %q", text)
	}
}

// TestStdioInteropWithHandwrittenClient 编译出 SDK server 二进制，
// 直接按手写 client 的方式往 stdin 灌 JSON-RPC 行——验证线上真实 stdio 形态
// 与手写协议栈的 client 完全互通。
func TestStdioInteropWithHandwrittenClient(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "coachstore-sdk.exe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "COACH_DB_PATH="+fixtureDB(t))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()

	lines := []string{
		`{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"handwritten-client","version":"0.1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`,
	}
	go func() {
		for _, l := range lines {
			stdin.Write([]byte(l + "\n"))
		}
	}()

	dec := json.NewDecoder(out)
	var sawResult bool
	for i := 0; i < 2; i++ { // initialize 响应 + tools/call 响应
		var msg struct {
			ID     any `json:"id"`
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
				Content         []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Result.ProtocolVersion != "" && msg.Result.ProtocolVersion != "2025-06-18" {
			// 版本向下协商是协议允许的，但握手必须成功
			t.Logf("协商版本: %s", msg.Result.ProtocolVersion)
		}
		if len(msg.Result.Content) > 0 && strings.Contains(msg.Result.Content[0].Text, "综合 88 分") {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("手写式 JSON-RPC 调用 list_sessions 未返回夹具数据")
	}
}
