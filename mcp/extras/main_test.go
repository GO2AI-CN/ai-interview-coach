package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newStubServer 造一个 ask 可替换的 server：单测不碰真实管道。
func newStubServer(ask func(method string, params any) (json.RawMessage, error)) *server {
	s := newServer(io.Discard)
	if ask != nil {
		s.ask = ask
	}
	return s
}

func dispatchOK(t *testing.T, s *server, id, method, params string) json.RawMessage {
	t.Helper()
	req := &rpcWire{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	resp := s.dispatch(req)
	if resp.Error != nil {
		t.Fatalf("%s: rpc error %+v", method, resp.Error)
	}
	return resp.Result
}

func TestPromptsListAndGet(t *testing.T) {
	s := newStubServer(nil)

	var list struct {
		Prompts []struct {
			Name      string `json:"name"`
			Arguments []struct {
				Name     string `json:"name"`
				Required bool   `json:"required"`
			} `json:"arguments"`
		} `json:"prompts"`
	}
	if err := json.Unmarshal(dispatchOK(t, s, `1`, "prompts/list", ""), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Prompts) != 1 || list.Prompts[0].Name != "review_session" {
		t.Fatalf("prompts/list = %+v", list)
	}

	got := dispatchOK(t, s, `2`, "prompts/get",
		`{"name":"review_session","arguments":{"report_md":"# 报告\n综合 72 分"}}`)
	var p struct {
		Messages []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != "user" ||
		!strings.Contains(p.Messages[0].Content.Text, "综合 72 分") {
		t.Errorf("prompts/get 组装失败: %+v", p)
	}
}

func TestPromptsGetValidation(t *testing.T) {
	s := newStubServer(nil)
	req := &rpcWire{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "prompts/get",
		Params: json.RawMessage(`{"name":"nope"}`)}
	if resp := s.dispatch(req); resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("unknown prompt want -32602, got %+v", resp.Error)
	}
	req.Params = json.RawMessage(`{"name":"review_session","arguments":{}}`)
	if resp := s.dispatch(req); resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("missing report_md want -32602, got %+v", resp.Error)
	}
}

// polish_report 的采样桩：记录收到的请求，返回固定文本。
func samplingStub(t *testing.T, got *map[string]any, err error) func(string, any) (json.RawMessage, error) {
	return func(method string, params any) (json.RawMessage, error) {
		if method != "sampling/createMessage" {
			t.Errorf("ask method = %s", method)
		}
		raw, _ := json.Marshal(params)
		if err := json.Unmarshal(raw, got); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			return nil, err
		}
		return mustRaw(map[string]any{
			"role":    "assistant",
			"content": map[string]any{"type": "text", "text": "润色后的复盘：发挥不错，并发继续练。"},
		}), nil
	}
}

func TestPolishReportViaSampling(t *testing.T) {
	var got map[string]any
	s := newStubServer(samplingStub(t, &got, nil))

	res := dispatchOK(t, s, `4`, "tools/call",
		`{"name":"polish_report","arguments":{"report_md":"# 报告"}}`)
	var out struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatal(err)
	}
	if out.IsError || out.Content[0].Text != "润色后的复盘：发挥不错，并发继续练。" {
		t.Errorf("polish result wrong: %+v", out)
	}
	// 断言采样请求的形状：messages + maxTokens 都必须在场
	if _, ok := got["messages"]; !ok {
		t.Errorf("sampling params 缺 messages: %v", got)
	}
	if got["maxTokens"].(float64) != 300 {
		t.Errorf("maxTokens = %v", got["maxTokens"])
	}
}

func TestPolishReportSamplingFailsGracefully(t *testing.T) {
	s := newStubServer(func(string, any) (json.RawMessage, error) {
		return nil, io.EOF // 模拟 client 拒绝/掉线
	})
	req := &rpcWire{JSONRPC: "2.0", ID: json.RawMessage(`5`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"polish_report","arguments":{"report_md":"x"}}`)}
	resp := s.dispatch(req)
	var out struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil || !out.IsError {
		t.Errorf("采样失败应回 isError, got %s", resp.Result)
	}
}

// TestInitializedTriggersRootsQuery 验证通知路由：initialized 后 server
// 会主动发起 roots/list（这里用桩 ask 接住，避免起真进程）。
func TestInitializedTriggersRootsQuery(t *testing.T) {
	asked := make(chan string, 1)
	s := newStubServer(func(method string, _ any) (json.RawMessage, error) {
		asked <- method
		return mustRaw(map[string]any{
			"roots": []any{map[string]any{"uri": "file:///workspace", "name": "workspace"}},
		}), nil
	})
	s.route([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	select {
	case m := <-asked:
		if m != "roots/list" {
			t.Errorf("ask = %s, want roots/list", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initialized 后未触发 roots/list")
	}
}

// ---- 以下为端到端：异步假 client，能应答 server 的反向请求 ----

type e2eClient struct {
	stdin    io.WriteCloser
	mu       sync.Mutex
	nextID   int
	pending  map[string]chan rpcWire
	serverMu sync.Mutex
	// serverAsked 记录 server 发来的所有反向请求（方法名），供断言
	serverAsked []string
}

func (c *e2eClient) call(method, params string) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcWire, 1)
	c.pending[itoa(id)] = ch
	line, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method,
		"params": json.RawMessage(params),
	})
	_, err := c.stdin.Write(append(line, '\n'))
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		return msg.Result, nil
	case <-time.After(5 * time.Second):
		return nil, io.EOF
	}
}

func itoa(i int) string {
	return string(rune('0' + i)) // e2e 里 id 不会超过 9，够用且无依赖
}

// serve 后台读循环：server→client 请求自动应答并记录；响应按 id 投递。
func (c *e2eClient) serve(t *testing.T, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var msg rpcWire
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			c.serverMu.Lock()
			c.serverAsked = append(c.serverAsked, msg.Method)
			c.serverMu.Unlock()
			var resp rpcWire
			resp.JSONRPC = "2.0"
			resp.ID = msg.ID
			switch msg.Method {
			case "roots/list":
				resp.Result = mustRaw(map[string]any{
					"roots": []any{map[string]any{"uri": "file:///workspace", "name": "demo"}},
				})
			case "sampling/createMessage":
				resp.Result = mustRaw(map[string]any{
					"role":       "assistant",
					"content":    map[string]any{"type": "text", "text": "润色后的复盘：发挥不错，并发继续练。"},
					"stopReason": "endTurn",
				})
			default:
				resp.Error = &rpcError{Code: -32601, Message: "unknown server request"}
			}
			line, _ := json.Marshal(resp)
			c.stdin.Write(append(line, '\n'))
		case msg.Method == "":
			c.mu.Lock()
			if ch, ok := c.pending[string(msg.ID)]; ok {
				ch <- msg
			}
			c.mu.Unlock()
		}
	}
}

func TestEndToEndBidirectional(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "extras.exe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Stderr = os.Stderr // 调试期看 server 日志；稳定后可去掉
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	c := &e2eClient{stdin: stdin, pending: map[string]chan rpcWire{}}
	go c.serve(t, stdout)

	initRes, err := c.call("initialize", `{
		"protocolVersion": "2025-06-18",
		"capabilities": {"sampling": {}, "roots": {}},
		"clientInfo": {"name": "e2e", "version": "0.1.0"}}`)
	if err != nil || !strings.Contains(string(initRes), "extras-mcp-server") {
		t.Fatalf("initialize: %v %s", err, initRes)
	}

	// initialized 通知：server 应主动查询 roots（serve 循环自动应答）
	if _, err := c.stdin.Write([]byte(
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	listRes, err := c.call("prompts/list", `{}`)
	if err != nil || !strings.Contains(string(listRes), "review_session") {
		t.Fatalf("prompts/list: %v %s", err, listRes)
	}

	toolRes, err := c.call("tools/call",
		`{"name":"polish_report","arguments":{"report_md":"# 报告\n综合 72 分"}}`)
	if err != nil || !strings.Contains(string(toolRes), "润色后的复盘") {
		t.Fatalf("tools/call: %v %s", err, toolRes)
	}

	// 等 roots 查询落账（server 侧是异步 goroutine）
	deadline := time.After(3 * time.Second)
	for {
		c.serverMu.Lock()
		n := len(c.serverAsked)
		methods := append([]string(nil), c.serverAsked...)
		c.serverMu.Unlock()
		if n >= 2 {
			joined := strings.Join(methods, ",")
			if !strings.Contains(joined, "roots/list") || !strings.Contains(joined, "sampling/createMessage") {
				t.Errorf("server 反向请求不全: %v", methods)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("server 反向请求不足 2 个: %v", methods)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
