// mcp/coachclient —— 手写 MCP client：spawn MCP server 子进程，经 stdio 完成
// initialize 握手，并把 tools/list、tools/call 包成 Go 方法（系列二第 5 篇配套）。
//
// 与 mcp/coachstore（服务端）对照阅读：两边消息骨架一致，
// client 多出的是「按 id 对账」和「跳过通知」两个动作。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// protocolVersion 与三个 server 保持一致。
const protocolVersion = "2025-06-18"

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcWire 是 client 侧的通用消息骨架。Result 用 RawMessage 原样接住，
// 交给 ListTools/CallTool 各自解析——client 不需要预知 result 的形状。
type rpcWire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Tool 是 tools/list 返回的工具描述，同时直接喂给大模型当函数定义。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client 是一个 MCP stdio 客户端。请求经 mutex 串行化：
// 响应按发出顺序回来，第一个响应必然对应当前请求，对账极简。
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Scanner
	mu     sync.Mutex
	nextID int
}

// NewClient 启动 server 子进程并完成握手。ctx 取消时子进程随 CommandContext 被杀掉。
func NewClient(ctx context.Context, bin string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ() // 继承环境变量：COACH_DB_PATH 等原样透传给 server
	cmd.Stderr = os.Stderr // server 的报错打到前台，排障时不用猜

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &Client{cmd: cmd, stdin: stdin}
	c.out = bufio.NewScanner(stdout)
	c.out.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// 握手：交换协议版本与双方身份。version 对不上要当场失败，别带病运行。
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "coachclient", "version": "0.1.0"},
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.ProtocolVersion == "" {
		c.Close()
		return nil, fmt.Errorf("initialize 响应缺 protocolVersion: %s", raw)
	}

	// 已初始化通知：协议要求发送，但按定义不应有响应，发了就走
	c.notify("notifications/initialized", map[string]any{})
	return c, nil
}

// call 发送一个 JSON-RPC 请求并阻塞到拿到同 id 的响应。
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := strconv.Itoa(c.nextID)
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	line, err := json.Marshal(rpcWire{JSONRPC: "2.0", ID: json.RawMessage(id),
		Method: method, Params: p})
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("写入请求失败（server 可能已退出）: %w", err)
	}

	for c.out.Scan() {
		var msg rpcWire
		if err := json.Unmarshal(c.out.Bytes(), &msg); err != nil {
			continue // 容错：跳过非 JSON 行（server 若有调试输出）
		}
		if len(msg.ID) == 0 {
			continue // 通知（notification）：没有 id，忽略
		}
		if string(msg.ID) != id {
			return nil, fmt.Errorf("响应 id 不匹配: got %s, want %s", msg.ID, id)
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		return msg.Result, nil
	}
	if err := c.out.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("server 进程已退出，未等到响应")
}

// notify 发送通知：不分配 id、不等响应，写失败也无需处理。
func (c *Client) notify(method string, params any) {
	p, _ := json.Marshal(params)
	line, _ := json.Marshal(rpcWire{JSONRPC: "2.0", Method: method, Params: p})
	_, _ = c.stdin.Write(append(line, '\n'))
}

// ListTools 拉取 server 的工具清单，转成 []Tool 供循环使用。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("解析 tools/list 失败: %w", err)
	}
	return res.Tools, nil
}

// CallTool 调用一个工具。isError=true 时把错误文本包装成 error 返回——
// 上层（循环）决定是把错误喂回模型还是中止，这里不做策略。
func (c *Client) CallTool(ctx context.Context, name string, arguments any) (string, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{
		"name": name, "arguments": arguments,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("解析 tools/call 结果失败: %w", err)
	}
	if len(res.Content) == 0 {
		return "", errors.New("工具返回空 content")
	}
	if res.IsError {
		return "", errors.New(res.Content[0].Text)
	}
	return res.Content[0].Text, nil
}

// Close 关管道并回收子进程。重复调用安全。
func (c *Client) Close() error {
	if c.stdin != nil {
		_ = c.stdin.Close() // 先关 stdin：server 读到 EOF 自然退出
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill() // 兜底：协议没规定退出消息，直接杀
	}
	_ = c.cmd.Wait()
	return nil
}
