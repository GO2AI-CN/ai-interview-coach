package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAILLM 用 openai-go v3 SDK 实现 LLM 接口。
// 认证与环境变量沿用系列一约定：OPENAI_API_KEY（SDK 自动读取）、
// OPENAI_BASE_URL（兼容 DeepSeek/Kimi 等 OpenAI 协议端点）、COACH_MODEL。
type OpenAILLM struct {
	cli   openai.Client // NewClient 返回值类型，不是指针
	model string
}

func NewOpenAILLM(baseURL, apiKey, model string) *OpenAILLM {
	return &OpenAILLM{
		cli: openai.NewClient(
			option.WithBaseURL(baseURL),
			option.WithAPIKey(apiKey),
		),
		model: model,
	}
}

// Chat 把循环的 Message/Tool 翻译成 SDK 类型，调用后发回。
// 双向翻译都集中在这一处：换供应商时只动这个文件。
func (l *OpenAILLM) Chat(ctx context.Context, messages []Message, tools []Tool) (*Message, error) {
	params := openai.ChatCompletionNewParams{Model: shared.ChatModel(l.model)}

	for _, m := range messages {
		switch m.Role {
		case "system":
			params.Messages = append(params.Messages, openai.SystemMessage(m.Text))
		case "user":
			params.Messages = append(params.Messages, openai.UserMessage(m.Text))
		case "tool":
			params.Messages = append(params.Messages, openai.ToolMessage(m.Text, m.ToolCallID))
		case "assistant":
			assistant := &openai.ChatCompletionAssistantMessageParam{
				Content: openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: param.NewOpt(m.Text),
				},
			}
			for _, tc := range m.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls,
					openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: string(tc.Arguments),
							},
						},
					})
			}
			params.Messages = append(params.Messages,
				openai.ChatCompletionMessageParamUnion{OfAssistant: assistant})
		}
	}

	// MCP 工具的 JSON Schema 原样映射为 function 工具的 parameters
	for _, t := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(
			shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  shared.FunctionParameters(t.InputSchema),
			}))
	}

	resp, err := l.cli.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("模型返回空 choices")
	}

	msg := resp.Choices[0].Message
	out := &Message{Role: "assistant", Text: msg.Content}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out, nil
}
