package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	// ChatStream 流式对话：逐 token 回调 onToken，返回完整文本。
	ChatStream(ctx context.Context, system, user string, onToken func(string)) (string, error)
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

func (c *OpenAIClient) chatParams(system, user string) openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system), openai.UserMessage(user)},
	}
}

func (c *OpenAIClient) ChatText(ctx context.Context, system, user string) (string, error) {
	resp, err := c.cli.Chat.Completions.New(ctx, c.chatParams(system, user))
	if err != nil {
		return "", fmt.Errorf("chat: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("chat: empty choices")
	}
	globalUsage.add(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	return resp.Choices[0].Message.Content, nil
}

// ChatStream 流式输出：SSE 逐 chunk 读取，边读边回调，用户体验"字往外蹦"。
func (c *OpenAIClient) ChatStream(ctx context.Context, system, user string, onToken func(string)) (string, error) {
	params := c.chatParams(system, user)
	// 流式的 usage 只在最后一个 chunk 里且默认不带，要显式开启
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: param.NewOpt(true),
	}
	stream := c.cli.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	var full strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		if chunk.JSON.Usage.Valid() { // 最后一个 chunk：整场请求的用量
			globalUsage.add(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
		}
		for _, choice := range chunk.Choices {
			if tok := choice.Delta.Content; tok != "" {
				full.WriteString(tok)
				if onToken != nil {
					onToken(tok) // 逐 token 推送
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return full.String(), fmt.Errorf("stream: %w", err)
	}
	return full.String(), nil
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
	globalUsage.add(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), out); err != nil {
		return fmt.Errorf("parse json output: %w (raw=%.120s)", err, resp.Choices[0].Message.Content)
	}
	return nil
}
