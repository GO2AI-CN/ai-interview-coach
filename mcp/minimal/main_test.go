package main

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"testing"
)

// rpcCall 直接调用分发器，验证握手返回的协议版本与服务器信息。
func TestInitializeHandshake(t *testing.T) {
	req := &rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"}
	resp := handleRequest(req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %T", resp.Result)
	}
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", result["protocolVersion"], protocolVersion)
	}
	info, ok := result["serverInfo"].(map[string]any)
	if !ok || info["name"] != "minimal-mcp-server" {
		t.Errorf("serverInfo missing or wrong: %v", result["serverInfo"])
	}
}

// 通知没有 id，服务端必须静默处理、不产生响应。
func TestNotificationGetsNoResponse(t *testing.T) {
	req := &rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	if resp := handleRequest(req); resp != nil {
		t.Errorf("notification should not produce a response, got %+v", resp)
	}
}

// tools/call 走通后，content 数组里应原样带回文本。
func TestToolsCallEcho(t *testing.T) {
	req := &rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"echo","arguments":{"text":"hello mcp"}}`),
	}
	resp := handleRequest(req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "hello mcp" {
		t.Errorf("echo content wrong: %v", first)
	}
}

func TestUnknownToolReturnsRPCError(t *testing.T) {
	req := &rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"nope","arguments":{}}`),
	}
	resp := handleRequest(req)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("want -32601, got %+v", resp.Error)
	}
}

// TestEndToEndSTDIO 构建真实二进制，通过管道模拟宿主发送一行请求、读一行响应，
// 验证 stdio 传输层 + 分发器的完整链路。
func TestEndToEndSTDIO(t *testing.T) {
	cmd := exec.Command("go", "run", ".")
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
	defer cmd.Process.Kill()

	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":9,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("no response from server")
	}
	var resp rpcResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v (%s)", err, scanner.Text())
	}
	if string(resp.ID) != "9" || resp.Error != nil {
		t.Errorf("unexpected response: %+v", resp)
	}
}
