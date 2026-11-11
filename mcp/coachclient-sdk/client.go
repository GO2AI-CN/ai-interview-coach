// mcp/coachclient-sdk —— 用官方 SDK 重写 mcp/coachclient（系列二第 8 篇预告的兑现）。
//
// 与手写版对照阅读：公开方法签名刻意保持完全一致（NewClient/ListTools/
// CallTool/Close），main 和循环代码一行不改就能换底——
// 这正是「手写学原理，SDK 进生产」的最好注脚。
//
// 依赖：github.com/modelcontextprotocol/go-sdk v1.8.0（MCP 官方 Go SDK）。
// 手写版 client.go 里的对账、通知跳过、进程回收，这里全部由 SDK 接管。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool 是 tools/list 返回的工具描述，同时直接喂给大模型当函数定义。
// 结构体与手写版完全一致——SDK 类型到它的转换集中在 ListTools 一处。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client 是对 *mcp.ClientSession 的薄封装，保持与手写 coachclient 相同的调用面。
type Client struct {
	session *mcp.ClientSession
}

// NewClient 启动 server 子进程并完成 initialize 握手。
// 相比手写版的三件麻烦事：协议版本协商、通知跳过、子进程回收——
// CommandTransport + Connect 一次全包（TerminateDuration 兜底杀进程）。
func NewClient(ctx context.Context, bin string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ() // 继承环境变量：COACH_DB_PATH 等原样透传给 server
	cmd.Stderr = os.Stderr // server 的报错打到前台，排障时不用猜

	transport := &mcp.CommandTransport{Command: cmd} // SDK 版「spawn 子进程」

	cli := mcp.NewClient(&mcp.Implementation{Name: "coachclient", Version: "0.1.0"}, nil)
	// Connect 内部完成握手：协议版本自动协商（向上支持到 2026-07-28）、
	// notifications/initialized 自动发送——手写版 NewClient 里那两段都没了。
	session, err := cli.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return &Client{session: session}, nil
}

// ListTools 拉取 server 的工具清单，转成手写版同款的 []Tool 喂给循环。
// SDK 返回的 InputSchema 是 any（map[string]any），原样塞进我们的结构体即可。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	res, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		schema, _ := t.InputSchema.(map[string]any) // server 手写版发的正是 map[string]any
		tools = append(tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return tools, nil
}

// CallTool 调用一个工具。协议约定工具错误走 isError 标记而非 JSON-RPC 错误，
// SDK 同样遵守——IsError=true 时把错误文本包装成 error 返回，
// 上层（循环）决定是把错误喂回模型还是中止，这里不做策略（与手写版一致）。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	var args map[string]any
	if len(arguments) > 0 {
		// 模型生成的参数是原始 JSON，先落成 map 再交给 SDK（SDK 会重新 marshal）
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "", fmt.Errorf("工具参数不是合法 JSON: %w", err)
		}
	}
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err // 传输层/协议层错误
	}
	if len(res.Content) == 0 {
		return "", errors.New("工具返回空 content")
	}
	text, ok := res.Content[0].(*mcp.TextContent) // 手写 server 只发文本块
	if !ok {
		return "", fmt.Errorf("非文本工具结果: %T", res.Content[0])
	}
	if res.IsError {
		return "", errors.New(text.Text)
	}
	return text.Text, nil
}

// Close 关闭会话。CommandTransport 先关 stdin 等 server 自然退出
// （默认等 5 秒，超时再发终止信号）——手写版的「关管道+Kill+Wait」三连在这里是一行。
func (c *Client) Close() error {
	return c.session.Close()
}
