// mcp/remote —— Streamable HTTP 传输的 MCP server + client（系列二第 7 篇配套）。
//
// 规范依据：MCP 协议版本 2025-06-18 的 Transports 章节。
// 要点实现：
//   - 单一 MCP 端点（默认 /mcp），POST 收发 JSON-RPC，GET 挂 SSE 收 server 推送；
//   - POST 请求的响应按 Accept 协商：application/json 单体，或 text/event-stream 流式；
//   - 通知/响应类消息 → 202 Accepted；
//   - 会话：initialize 响应带 Mcp-Session-Id，后续请求必须携带；
//     缺会话 → 400，会话失效 → 404，DELETE 显式终止；
//   - MCP-Protocol-Version 请求头：不支持 → 400；
//   - 安全三件套（规范 Security Warning）：Bearer 鉴权、Origin 校验（防 DNS rebinding）、
//     默认只绑 127.0.0.1。
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// protocolVersion 与全系列保持一致。服务器也用它校验 MCP-Protocol-Version 头。
const protocolVersion = "2025-06-18"

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcWire 双向通用消息骨架。
type rpcWire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}

// streamServer 是承载 Streamable HTTP 传输的服务器。
// 业务分发（dispatch）刻意保持极简——本篇重点是传输层，不是工具逻辑。
type streamServer struct {
	token string

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	// streams 是该会话挂起的 GET SSE 流，server 推送时逐个非阻塞投递
	mu      sync.Mutex
	streams []chan string
}

func newStreamServer(token string) *streamServer {
	return &streamServer{token: token, sessions: map[string]*session{}}
}

// checkAuth 校验 Bearer token，常量时间比较防时序侧信道。
func (s *streamServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer realm=mcp")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

// checkOrigin 按规范要求校验 Origin 头，防 DNS rebinding：
// 浏览器跨站请求会带 Origin，本地 server 只放行本机来源；无 Origin（非浏览器客户端）放行。
func checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := origin
	if i := strings.Index(origin, "://"); i >= 0 {
		host = origin[i+3:]
	}
	host = strings.Split(host, ":")[0]
	if host == "localhost" || host == "127.0.0.1" {
		return true
	}
	http.Error(w, `{"error":"origin forbidden"}`, http.StatusForbidden)
	return false
}

// checkProtocolVersion 校验 MCP-Protocol-Version 头：不支持的版本必须 400。
func checkProtocolVersion(w http.ResponseWriter, r *http.Request) bool {
	v := r.Header.Get("MCP-Protocol-Version")
	if v != "" && v != protocolVersion {
		http.Error(w, `{"error":"unsupported protocol version"}`, http.StatusBadRequest)
		return false
	}
	return true
}

// checkSession 返回会话；initialize 之外的请求必须带有效会话头。
func (s *streamServer) checkSession(w http.ResponseWriter, r *http.Request) *session {
	id := r.Header.Get("Mcp-Session-Id")
	if id == "" {
		http.Error(w, `{"error":"Mcp-Session-Id required"}`, http.StatusBadRequest)
		return nil
	}
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return nil
	}
	return sess
}

func (s *streamServer) newSession() (string, *session) {
	buf := make([]byte, 16)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	sess := &session{}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return id, sess
}

func (s *streamServer) dropSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// ServeHTTP 是单一 MCP 端点：同时承载 POST（client→server）与 GET（server→client SSE）。
func (s *streamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) || !checkOrigin(w, r) || !checkProtocolVersion(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (s *streamServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("Mcp-Session-Id")
	s.dropSession(id)
	w.WriteHeader(http.StatusOK)
}

// handleGet 挂起一条 SSE 流，供 server 主动推送（notifications / 反向请求）。
func (s *streamServer) handleGet(w http.ResponseWriter, r *http.Request) {
	sess := s.checkSession(w, r)
	if sess == nil {
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, `{"error":"SSE not accepted"}`, http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, ": connected\n\n") // SSE 注释行，充当连接确认
	flusher.Flush()

	ch := make(chan string, 8)
	sess.mu.Lock()
	sess.streams = append(sess.streams, ch)
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		for i, c := range sess.streams {
			if c == ch {
				sess.streams = append(sess.streams[:i], sess.streams[i+1:]...)
				break
			}
		}
		sess.mu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprint(w, msg)
			flusher.Flush()
		case <-time.After(25 * time.Second): // 心跳注释，防代理断连
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handlePost 处理每条 client→server 的 JSON-RPC 消息（一条消息一次 POST）。
func (s *streamServer) handlePost(w http.ResponseWriter, r *http.Request) {
	var msg rpcWire
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`,
			http.StatusBadRequest)
		return
	}

	// 通知/响应：收下入队即可，回 202（规范要求）
	if msg.Method == "" || len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// initialize 是唯一能「无会话进入」的请求
	if msg.Method == "initialize" {
		sessID, _ := s.newSession()
		w.Header().Set("Mcp-Session-Id", sessID)
		s.writeResult(w, r, dispatch(&msg))
		return
	}

	sess := s.checkSession(w, r)
	if sess == nil {
		return
	}
	resp := dispatch(&msg)
	s.writeResult(w, r, resp)
	// tools/call 成功后向该会话的 GET 流推一条通知：演示 server 主动推送
	if msg.Method == "tools/call" && resp.Error == nil {
		s.push(sess, fmt.Sprintf(
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/called\",\"params\":{\"name\":%s}}\n\n",
			mustRaw(toolName(msg.Params))))
	}
}

// toolName 从 tools/call 参数里抠出工具名，给推送通知用。
func toolName(params json.RawMessage) string {
	var in struct {
		Name string `json:"name"`
	}
	json.Unmarshal(params, &in)
	return in.Name
}

// writeResult 按 Accept 协商输出：SSE 或单体 JSON。
// 规范：SSE 模式下也必须最终带上本次请求的 JSON-RPC 响应，然后关流。
func (s *streamServer) writeResult(w http.ResponseWriter, r *http.Request, resp *rpcWire) {
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// push 向会话内所有挂起的 GET 流非阻塞推送一条 SSE 消息；慢消费者直接丢，不拖垮 server。
func (s *streamServer) push(sess *session, msg string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, ch := range sess.streams {
		select {
		case ch <- msg:
		default:
		}
	}
}

// dispatch 是极简业务分发：本篇主角是传输层，工具只留一个 greet 做验证。
func dispatch(req *rpcWire) *rpcWire {
	resp := &rpcWire{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = mustRaw(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "remote-mcp-server", "version": "0.7.0"},
		})
	case "ping":
		resp.Result = mustRaw(map[string]any{})
	case "tools/list":
		resp.Result = mustRaw(map[string]any{
			"tools": []any{map[string]any{
				"name":        "greet",
				"description": "打个招呼，验证远程链路",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"name": map[string]any{"type": "string"}},
					"required":   []string{"name"},
				},
			}},
		})
	case "tools/call":
		var in struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &in); err != nil || in.Name != "greet" {
			resp.Error = &rpcError{Code: -32601, Message: "unknown tool"}
			return resp
		}
		var args struct{ Name string }
		if err := json.Unmarshal(in.Arguments, &args); err != nil || args.Name == "" {
			resp.Error = &rpcError{Code: -32602, Message: "name is required"}
			return resp
		}
		resp.Result = mustRaw(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "你好, " + args.Name + "！远程 MCP 链路已通。"}},
		})
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
