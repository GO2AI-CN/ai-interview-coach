package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ai-interview-coach/internal/bot"
	"ai-interview-coach/internal/store"
)

// newTestServer 用 store 包造一个带真实数据的历史库，再让 server 以只读方式打开它。
// 测试经 store.Open + Save 写数据、经 openDB 读数据：两边走同一个 SQLite 文件，
// 等于顺带验证了两个包对 schema 的理解一致。
func newTestServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "coach.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	turns := []bot.Turn{
		{Dimension: "并发", Question: "goroutine 调度模型？", Answer: "GMP……",
			Confidence: 4, Coverage: 0.75, FollowUps: 2},
		{Dimension: "切片", Question: "append 扩容策略？", Answer: "……",
			Confidence: 5, Coverage: 1, FollowUps: 0},
	}
	if _, err := st.Save(ctx, "feishu:g-1", "Go 后端工程师", 72, "# 第一场报告", turns); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(ctx, "feishu:g-1", "SRE", 91, "# 第二场报告", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &server{db: db}, path
}

func callTool(t *testing.T, s *server, id int, name, args string) *rpcResponse {
	t.Helper()
	params := `{"name":"` + name + `"`
	if args != "" {
		params += `,"arguments":` + args
	}
	params += `}`
	req := &rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)),
		Method: "tools/call", Params: json.RawMessage(params)}
	return s.handleRequest(req)
}

func toolText(t *testing.T, resp *rpcResponse) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["isError"] == true {
		t.Fatalf("unexpected business error: %v", result["content"])
	}
	content := result["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

// query_only 是硬保证：任何写操作都必须被拒绝，这是本篇的核心安全承诺。
func TestQueryOnlyBlocksWrites(t *testing.T) {
	s, _ := newTestServer(t)
	_, err := s.db.Exec(`DELETE FROM interviews`)
	if err == nil {
		t.Fatal("query_only=ON 下 DELETE 竟然成功了")
	}
	if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "attempt to write") {
		t.Fatalf("错误信息不像只读拒绝: %v", err)
	}
}

func TestListSessionsNewestFirst(t *testing.T) {
	s, _ := newTestServer(t)
	text := toolText(t, callTool(t, s, 1, "list_sessions", `{}`))
	first := strings.SplitN(text, "\n", 2)[1] // 跳过表头行
	if !strings.HasPrefix(first, "#2") {
		t.Errorf("最新场次应为 #2，got line: %q", first)
	}
	if !strings.Contains(first, "SRE") || !strings.Contains(first, "91") {
		t.Errorf("场次信息不完整: %q", first)
	}
}

func TestListSessionsLimitClamped(t *testing.T) {
	s, _ := newTestServer(t)
	// limit 超过上限被截到 50；非法值不报错（对模型友好）
	text := toolText(t, callTool(t, s, 2, "list_sessions", `{"limit":9999}`))
	if strings.Count(text, "#1")+strings.Count(text, "#2") < 2 {
		t.Errorf("limit=9999 应截断后返回全部 2 场: %q", text)
	}
	text = toolText(t, callTool(t, s, 3, "list_sessions", `{"limit":-5}`))
	if !strings.Contains(text, "#2") {
		t.Errorf("limit=-5 应回退默认值: %q", text)
	}
}

func TestGetReport(t *testing.T) {
	s, _ := newTestServer(t)
	text := toolText(t, callTool(t, s, 4, "get_report", `{"id":1}`))
	if !strings.Contains(text, "第一场报告") {
		t.Errorf("报告内容不对: %q", text)
	}
}

func TestGetReportUnknownIDIsBusinessError(t *testing.T) {
	s, _ := newTestServer(t)
	resp := callTool(t, s, 5, "get_report", `{"id":999}`)
	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Fatalf("unknown id 应是 isError, got %v", result)
	}
	if !strings.Contains(toolTextOf(t, result), "不存在") {
		t.Errorf("错误文本应提示先 list_sessions: %v", result["content"])
	}
}

func toolTextOf(t *testing.T, result map[string]any) string {
	t.Helper()
	content := result["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

func TestGetSessionDetail(t *testing.T) {
	s, _ := newTestServer(t)
	text := toolText(t, callTool(t, s, 6, "get_session", `{"id":1}`))
	if !strings.Contains(text, "综合 72 分") {
		t.Errorf("header 缺少总分: %q", text)
	}
	if !strings.Contains(text, "[并发]") || !strings.Contains(text, "覆盖度75%") || !strings.Contains(text, "追问2次") {
		t.Errorf("逐题指标不完整: %q", text)
	}
	if strings.Contains(text, "GMP") {
		t.Errorf("get_session 不应泄露答案原文（那是 get_report 的职责）: %q", text)
	}
}

func TestEmptyDBIsBusinessError(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db}
	resp := callTool(t, s, 7, "list_sessions", `{}`)
	if resp.Result.(map[string]any)["isError"] != true {
		t.Errorf("空库应 isError, got %v", resp.Result)
	}
}

// TestEndToEndSTDIO 编译出二进制再起真实进程（Windows 下 kill go run 的父进程
// 杀不掉子进程，还会占着 SQLite 文件句柄），经 COACH_DB_PATH 指向夹具库验证全链路。
func TestEndToEndSTDIO(t *testing.T) {
	_, path := newTestServer(t)

	bin := filepath.Join(t.TempDir(), "coachstore-test.exe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "COACH_DB_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_report","arguments":{"id":2}}}`,
	}
	for _, l := range lines {
		if _, err := stdin.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}

	scanner := bufio.NewScanner(stdout)
	responses := map[string]map[string]any{}
	for len(responses) < 4 { // 通知不应产生响应
		if !scanner.Scan() {
			t.Fatalf("expected 4 responses, got %d", len(responses))
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result map[string]any  `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v (%s)", err, scanner.Text())
		}
		responses[string(resp.ID)] = resp.Result
	}

	if tools := responses["2"]["tools"].([]any); len(tools) != 3 {
		t.Errorf("e2e: tools/list = %d, want 3", len(tools))
	}
	if text := responses["3"]["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "#2") {
		t.Errorf("e2e: list_sessions should contain #2: %q", text)
	}
	if text := responses["4"]["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "第二场报告") {
		t.Errorf("e2e: get_report should return report 2: %q", text)
	}

	// 显式结束进程并等待句柄释放，否则 Windows 上 TempDir 清理会删不掉 db 文件
	if err := cmd.Process.Kill(); err != nil {
		t.Logf("kill: %v", err)
	}
	_ = cmd.Wait()
}
