package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Message 是与模型往返的统一消息结构。一个结构同时容纳四种角色：
// system / user 纯文本；assistant 可能带工具调用请求；tool 是工具结果回执。
// 用结构体而非各家 SDK 的类型，是为了让循环本身与 LLM 供应商解耦。
type Message struct {
	Role       string     // system | user | assistant | tool
	Text       string     // 文本内容（assistant 的 ToolCalls 为空时即最终答复）
	ToolCallID string     // role=tool 时必填：回应哪一次调用
	ToolCalls  []ToolCall // role=assistant 且有工具调用时非空
}

// ToolCall 是模型发起的一次工具调用请求。
type ToolCall struct {
	ID        string          // 模型生成的调用 id，结果回执要原样带回
	Name      string          // 工具名
	Arguments json.RawMessage // 模型生成的参数（未校验，执行侧负责兜底）
}

// LLM 是模型适配接口：给历史与工具清单，返回一轮输出（文本或调用请求）。
// 接真实模型（openai_llm.go）或测试替身（fake）都实现它。
// 注意 Tool 类型直接复用 client.go 里的定义——同包内不需要再造一份。
type LLM interface {
	Chat(ctx context.Context, messages []Message, tools []Tool) (*Message, error)
}

// Executor 执行一次工具调用，返回喂回模型的文本结果。
type Executor func(ctx context.Context, name string, args json.RawMessage) (string, error)

// RunLoop 函数调用循环（function calling loop）：
//
//	轮询模型 → 要调工具就执行、把结果作为 tool 消息追加 → 再轮询 → …
//	直到模型给出不带工具调用的纯文本答复，或撞 maxSteps 上限。
//
// 核心设计：工具执行失败不中止循环，而是把错误文本作为工具结果喂回模型——
// 上篇的 isError 引路文案就是给这一刻准备的，模型读到会自己换参数重试。
func RunLoop(ctx context.Context, llm LLM, exec Executor,
	system, user string, tools []Tool, maxSteps int) (string, error) {
	messages := []Message{
		{Role: "system", Text: system},
		{Role: "user", Text: user},
	}

	for step := 1; step <= maxSteps; step++ {
		resp, err := llm.Chat(ctx, messages, tools)
		if err != nil {
			return "", fmt.Errorf("第 %d 轮对话失败: %w", step, err)
		}
		if resp.Role != "assistant" {
			resp.Role = "assistant" // 防御：适配器漏标角色时兜底
		}

		if len(resp.ToolCalls) == 0 {
			return resp.Text, nil // 纯文本 = 最终答复，收工
		}

		messages = append(messages, *resp)
		for _, tc := range resp.ToolCalls {
			result, err := exec(ctx, tc.Name, tc.Arguments)
			if err != nil {
				result = "工具调用失败：" + err.Error()
			}
			messages = append(messages, Message{
				Role: "tool", ToolCallID: tc.ID, Text: result,
			})
		}
	}
	return "", fmt.Errorf("超过最大轮数 %d，模型仍在请求工具调用", maxSteps)
}

// compile-time check：scriptedLLM / OpenAILLM 都必须满足 LLM 接口
var (
	_ LLM = (*scriptedLLM)(nil)
	_ LLM = (*OpenAILLM)(nil)
)
