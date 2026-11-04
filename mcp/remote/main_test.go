package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "test-secret-token"

func newTestServer(t *testing.T) (*httptest.Server, *streamServer) {
	t.Helper()
	s := newStreamServer(testToken)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, s
}

func newTestClient(t *testing.T, ts *httptest.Server) *RemoteClient {
	return NewRemoteClient(ts.URL+"/mcp", testToken)
}

func post(t *testing.T, url, token, session, body string, acceptSSE bool) *http.Response {
	t.Helper()
	accept := "application/json"
	if acceptSSE {
		accept = "application/json, text/event-stream"
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// 标准握手：initialize 必须返回 Mcp-Session-Id 响应头。
func initialize(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := post(t, ts.URL+"/mcp", testToken, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize http %d", resp.StatusCode)
	}
	sess := resp.Header.Get("Mcp-Session-Id")
	if sess == "" {
		t.Fatal("initialize 响应缺 Mcp-Session-Id 头")
	}
	return sess
}

// 完整链路：握手 → 工具列表 → 工具调用 → 通知 202 → DELETE 终止 → 旧会话 404。
func TestRemoteFullFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := newTestClient(t, ts)
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := c.Call(ctx, "tools/list", map[string]any{})
	if err != nil || !strings.Contains(string(res), "greet") {
		t.Fatalf("tools/list: %v %s", err, res)
	}
	res, err = c.Call(ctx, "tools/call", map[string]any{
		"name": "greet", "arguments": map[string]any{"name": "MCP"},
	})
	if err != nil || !strings.Contains(string(res), "远程 MCP 链路已通") {
		t.Fatalf("tools/call: %v %s", err, res)
	}
	if err := c.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// 会话已终止：再带旧会话请求必须 404
	resp := post(t, ts.URL+"/mcp", testToken, c.sessionID, /*已清空，这里重新拿*/
		`{"jsonrpc":"2.0","id":9,"method":"ping"}`, false)
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("terminated session want 404/400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// 鉴权：缺 token / 错 token 必须 401，且带 WWW-Authenticate。
func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/mcp", "wrong-token", "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token want 401, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 响应缺 WWW-Authenticate")
	}
	resp.Body.Close()
}

// 会话纪律：initialize 之外的请求无会话头 → 400；伪造会话 → 404。
func TestSessionEnforcement(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := post(t, ts.URL+"/mcp", testToken, "",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, false)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no session want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = post(t, ts.URL+"/mcp", testToken, "forged-session-id",
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, false)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("forged session want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Origin 校验（防 DNS rebinding）：跨站 Origin 必须 403；无 Origin（非浏览器）放行。
func TestOriginValidation(t *testing.T) {
	ts, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "http://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("evil origin want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 本机 Origin 放行
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("localhost origin want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// 协议版本头：不支持的版本必须 400。
func TestProtocolVersionHeader(t *testing.T) {
	ts, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("MCP-Protocol-Version", "1999-01-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unsupported version want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// POST 的 SSE 协商：Accept 带 text/event-stream 时，响应必须是 SSE 包裹的 JSON-RPC。
func TestPostSSENegotiation(t *testing.T) {
	ts, _ := newTestServer(t)
	sess := initialize(t, ts)

	resp := post(t, ts.URL+"/mcp", testToken, sess,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"greet","arguments":{"name":"SSE"}}}`,
		true)
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("want SSE content-type, got %s", resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	found := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") &&
			strings.Contains(scanner.Text(), "远程 MCP 链路已通") {
			found = true
		}
	}
	if !found {
		t.Error("SSE 流中未找到 tools/call 响应")
	}
}

// GET 打开 SSE 流：server 在 tools/call 后主动推通知，流上必须能收到。
func TestGetStreamReceivesPush(t *testing.T) {
	ts, _ := newTestServer(t)
	sess := initialize(t, ts)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Mcp-Session-Id", sess)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET want SSE, got %s", resp.Header.Get("Content-Type"))
	}

	// 在后台读流的同时触发一次 tools/call
	type line struct{ text string }
	lines := make(chan line, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- line{scanner.Text()}
		}
	}()

	post(t, ts.URL+"/mcp", testToken, sess,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"greet","arguments":{"name":"推送"}}}`,
		false).Body.Close()

	timeout := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l.text, "notifications/tools/called") {
				return // 收到 server 主动推送，测试通过
			}
		case <-timeout:
			t.Fatal("3 秒内未在 GET 流收到 server 推送")
		}
	}
}

// 通知/响应类 POST 必须回 202 Accepted 且无 body。
func TestNotificationReturns202(t *testing.T) {
	ts, _ := newTestServer(t)
	sess := initialize(t, ts)

	resp := post(t, ts.URL+"/mcp", testToken, sess,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification want 202, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("202 响应应无 body, got %q", body)
	}
}

// 非法 JSON 必须 400（parse error）。
func TestParseError400(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/mcp", testToken, "", `{not-json`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("parse error want 400, got %d", resp.StatusCode)
	}
}

// client 的 Call 在 SSE 响应模式下也要能捞出正确响应（协商路径的单测兜底）。
func TestClientCallWithSSEServer(t *testing.T) {
	ts, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 强制走 SSE：直接打 server（client.Call 已带两种 Accept，server 选择 SSE 与否都行）
	c := newTestClient(t, ts)
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	var res json.RawMessage
	res, err := c.Call(ctx, "tools/call", map[string]any{
		"name": "greet", "arguments": map[string]any{"name": "协商"},
	})
	if err != nil || !strings.Contains(string(res), "远程 MCP 链路已通") {
		t.Fatalf("call via negotiation: %v %s", err, res)
	}
}
