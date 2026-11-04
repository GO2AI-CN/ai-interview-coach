package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// RemoteClient 是 Streamable HTTP 传输的 MCP client。
// 与 stdio client 的最大区别：每个 JSON-RPC 消息都是独立 POST，
// 响应可能是单体 JSON，也可能是 SSE 流——两种都必须会读（规范强制）。
type RemoteClient struct {
	endpoint  string
	token     string
	sessionID string

	mu     sync.Mutex
	nextID int
	cli    *http.Client
}

func NewRemoteClient(endpoint, token string) *RemoteClient {
	return &RemoteClient{
		endpoint: endpoint,
		token:    token,
		cli:      &http.Client{Timeout: 0}, // 单请求超时由 ctx 控制；SSE 流不受 Timeout 限制
	}
}

// Call 发送一个 JSON-RPC 请求：initialize 之外自动带会话头与协议版本头。
// 返回 result 部分；server 返回 JSON-RPC error 时包装为 Go error。
func (c *RemoteClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	var idem struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}

	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}

	resp, err := c.post(ctx, reqBody, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, body)
	}

	// 协商结果二选一：SSE 流或单体 JSON
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		if err := c.readSSEResponse(resp.Body, id, &idem); err != nil {
			return nil, err
		}
	} else {
		if err := json.NewDecoder(resp.Body).Decode(&idem); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
	}

	if idem.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", idem.Error.Code, idem.Error.Message)
	}
	return idem.Result, nil
}

// post 统一组 POST 请求：三个规范头（Accept / Authorization / MCP-Protocol-Version）
// 加条件会话头。
func (c *RemoteClient) post(ctx context.Context, body []byte, acceptSSE bool) (*http.Response, error) {
	accept := "application/json"
	if acceptSSE {
		accept = "application/json, text/event-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	return c.cli.Do(req)
}

// Initialize 完成握手并捕获会话 id（Mcp-Session-Id 响应头）。
func (c *RemoteClient) Initialize(ctx context.Context) error {
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "remote-client", "version": "0.1.0"},
		},
	})
	resp, err := c.post(ctx, reqBody, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("initialize http %d: %s", resp.StatusCode, body)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	c.sessionID = resp.Header.Get("Mcp-Session-Id")
	if c.sessionID == "" {
		return errors.New("initialize 响应缺 Mcp-Session-Id")
	}
	return nil
}

// Notify 发通知（规范：server 回 202，无 body）。
func (c *RemoteClient) Notify(ctx context.Context, method string, params any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	resp, err := c.post(ctx, body, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("notify http %d", resp.StatusCode)
	}
	return nil
}

// Close 显式终止会话（DELETE + Mcp-Session-Id）。
func (c *RemoteClient) Close(ctx context.Context) error {
	if c.sessionID == "" {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	resp, err := c.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.sessionID = ""
	return nil
}

// readSSEResponse 从 SSE 响应里捞出属于本请求的 JSON-RPC 响应。
// 规范允许 server 在响应前先推别的消息（请求/通知），
// 所以这里要按 event 逐条分拣，而不是傻等第一行。
func (c *RemoteClient) readSSEResponse(body io.Reader, wantID int, out *struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}) error {
	scanner := bufio.NewScanner(body)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() == 0 {
				continue
			}
			// 一条 SSE 事件收齐：看它的 id 是否匹配本次请求
			var msg struct {
				ID json.RawMessage `json:"id"`
			}
			raw := data.String()
			data.Reset()
			if err := json.Unmarshal([]byte(raw), &msg); err != nil {
				continue // server 推的通知/请求没有 id 或形状不同，跳过
			}
			if string(msg.ID) == strconv.Itoa(wantID) {
				return json.Unmarshal([]byte(raw), out)
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		case strings.HasPrefix(line, ":"):
			// 注释行（心跳/连接确认），忽略
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("SSE 流关闭但未等到本请求的响应")
}
