package main

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func mustCall(t *testing.T, id int, method, params string) *rpcResponse {
	t.Helper()
	req := &rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: method}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	resp := handleRequest(req)
	if resp == nil {
		t.Fatalf("%s: got nil response", method)
	}
	return resp
}

func textContent(t *testing.T, resp *rpcResponse) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

// initialize 必须如实声明 tools 与 resources 两项能力。
func TestInitializeDeclaresCapabilities(t *testing.T) {
	resp := mustCall(t, 1, "initialize", "")
	caps := resp.Result.(map[string]any)["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities.tools missing")
	}
	if _, ok := caps["resources"]; !ok {
		t.Error("capabilities.resources missing")
	}
}

// tools/list 应包含注册表中的全部三个工具，且按名字排序。
func TestToolsListFromRegistry(t *testing.T) {
	resp := mustCall(t, 2, "tools/list", "")
	tools := resp.Result.(map[string]any)["tools"].([]any)
	if len(tools) != len(registry) {
		t.Fatalf("tools/list = %d, want %d", len(tools), len(registry))
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if names[0] != "calc" || names[1] != "list_categories" || names[2] != "query_question" {
		t.Errorf("tools not sorted: %v", names)
	}
}

func TestCalcSuccess(t *testing.T) {
	resp := mustCall(t, 3, "tools/call", `{"name":"calc","arguments":{"a":7,"b":6,"op":"*"}}`)
	if textContent(t, resp) != "42" {
		t.Errorf("7*6 should be 42, got %q", textContent(t, resp))
	}
}

// 除零是业务错误：响应不带 JSON-RPC error，而是 isError=true + 错误文本。
func TestCalcDivideByZeroIsBusinessError(t *testing.T) {
	resp := mustCall(t, 4, "tools/call", `{"name":"calc","arguments":{"a":1,"b":0,"op":"/"}}`)
	if resp.Error != nil {
		t.Fatalf("div-by-zero must not be a protocol error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Errorf("want isError=true, got %v", result)
	}
	if !strings.Contains(textContent(t, resp), "除数不能为 0") {
		t.Errorf("error text missing: %q", textContent(t, resp))
	}
}

// 缺必填字段同样走业务错误通道。
func TestCalcMissingOperand(t *testing.T) {
	resp := mustCall(t, 5, "tools/call", `{"name":"calc","arguments":{"a":1,"op":"+"}}`)
	if resp.Result.(map[string]any)["isError"] != true {
		t.Errorf("missing operand should be isError, got %+v", resp.Result)
	}
}

func TestQueryQuestionFilter(t *testing.T) {
	resp := mustCall(t, 6, "tools/call", `{"name":"query_question","arguments":{"category":"golang","difficulty":"medium"}}`)
	text := textContent(t, resp)
	if !strings.Contains(text, "channel 关闭后") {
		t.Errorf("expected golang/medium question, got %q", text)
	}
	if strings.Contains(text, "短链接") { // system-design 的不该出现
		t.Errorf("filter leaked other categories: %q", text)
	}
}

func TestQueryQuestionNoMatch(t *testing.T) {
	resp := mustCall(t, 7, "tools/call", `{"name":"query_question","arguments":{"category":"rust"}}`)
	if resp.Result.(map[string]any)["isError"] != true {
		t.Errorf("no match should be isError, got %+v", resp.Result)
	}
}

func TestResourcesListAndRead(t *testing.T) {
	listResp := mustCall(t, 8, "resources/list", "")
	rs := listResp.Result.(map[string]any)["resources"].([]any)
	if len(rs) != len(resources) {
		t.Fatalf("resources/list = %d, want %d", len(rs), len(resources))
	}

	uri := rs[0].(map[string]any)["uri"].(string)
	readResp := mustCall(t, 9, "resources/read", `{"uri":"`+uri+`"}`)
	result := readResp.Result.(map[string]any)
	contents := result["contents"].([]any)
	text := contents[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "# ") {
		t.Errorf("resource text should be markdown starting with '# ', got %q", text[:min(20, len(text))])
	}
}

func TestReadUnknownResource(t *testing.T) {
	resp := mustCall(t, 10, "resources/read", `{"uri":"interview://nope"}`)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("unknown resource want -32602, got %+v", resp.Error)
	}
}

// TestEndToEndSTDIO 起真实进程走 stdio 管道，验证注册表 + 能力声明的全链路。
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

	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
	}
	for _, l := range lines {
		if _, err := stdin.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}

	scanner := bufio.NewScanner(stdout)
	responses := map[string]rpcResponse{}
	for len(responses) < 3 { // 通知不应产生响应
		if !scanner.Scan() {
			t.Fatal("expected 3 responses")
		}
		var resp rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v (%s)", err, scanner.Text())
		}
		responses[string(resp.ID)] = resp
	}

	var initResp rpcResponse
	if err := json.Unmarshal(mustRaw(t, responses["1"]), &initResp); err != nil {
		t.Fatal(err)
	}
	caps := initResp.Result.(map[string]any)["capabilities"].(map[string]any)
	if _, ok := caps["resources"]; !ok {
		t.Error("e2e: capabilities.resources missing")
	}
	if len(responses["2"].Result.(map[string]any)["tools"].([]any)) != 3 {
		t.Error("e2e: tools/list should return 3 tools")
	}
	if len(responses["3"].Result.(map[string]any)["resources"].([]any)) != 2 {
		t.Error("e2e: resources/list should return 2 resources")
	}
}

// mustRaw 重新序列化响应，方便按 id 取出后再解析一次。
func mustRaw(t *testing.T, resp rpcResponse) []byte {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
