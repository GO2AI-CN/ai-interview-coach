// Package llm 封装大模型调用：JSON 模式 + 纯文本模式。
// Client 是接口，方便测试时用 Mock 替换。
package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// Client 抽象 LLM 能力，生产实现是 OpenAIClient，测试里用 Mock。
type Client interface {
	// ChatJSON 以 JSON Schema 约束输出并解析进 out（out 传指针）。
	ChatJSON(ctx context.Context, system, user, schemaName, schemaDesc string, schema any, out any) error
	// ChatText 普通对话，返回文本。
	ChatText(ctx context.Context, system, user string) (string, error)
}

// OpenAIClient 基于 openai-go v3 的实现，兼容任何 OpenAI 协议端点
// （OpenAI / DeepSeek / Kimi 等），通过环境变量切换：
// OPENAI_API_KEY / OPENAI_BASE_URL / COACH_MODEL。
type OpenAIClient struct {
	cli   openai.Client
	model string
}

func NewOpenAIClient(model string) *OpenAIClient {
	if model == "" {
		model = "gpt-4o-mini" // 兜底默认，建议用 COACH_MODEL 覆盖
	}
	return &OpenAIClient{cli: openai.NewClient(), model: model}
}

// NewOpenAIClientWithBaseURL 显式指定 baseURL（如 https://api.deepseek.com/v1）。
func NewOpenAIClientWithBaseURL(model, baseURL, apiKey string) *OpenAIClient {
	return &OpenAIClient{
		cli:   openai.NewClient(option.WithBaseURL(baseURL), option.WithAPIKey(apiKey)),
		model: model,
	}
}

func (c *OpenAIClient) ChatText(ctx context.Context, system, user string) (string, error) {
	resp, err := c.cli.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system), openai.UserMessage(user)},
	})
	if err != nil {
		return "", fmt.Errorf("chat: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("chat: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}

func (c *OpenAIClient) ChatJSON(ctx context.Context, system, user, schemaName, schemaDesc string, schema any, out any) error {
	resp, err := c.cli.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system), openai.UserMessage(user)},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        schemaName,
					Description: param.NewOpt(schemaDesc),
					Schema:      schema,
					Strict:      param.NewOpt(true),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("chat json: %w", err)
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("chat json: empty choices")
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), out); err != nil {
		return fmt.Errorf("parse json output: %w (raw=%.120s)", err, resp.Choices[0].Message.Content)
	}
	return nil
}
