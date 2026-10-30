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

// ChatJSON 优先用 strict JSON Schema 约束输出；部分端点（如 DeepSeek）不支持
// json_schema，会返回 400，此时按 json_object → 纯提示词两级降级，保证兼容。
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
	// 降级时端点看不到 schema 参数，要把 Schema 以文本形式补进提示词，
	// 否则模型不知道有哪些字段（dimension 等就会出现空值）。
	schemaText, _ := json.MarshalIndent(schema, "", "  ")
	jsonRule := "\n\n只输出一个 JSON 对象，不要输出任何其他文字、解释或 Markdown 代码块。输出必须符合这个 JSON Schema：\n" + string(schemaText)

	if err != nil {
		// 降级 1：json_object（DeepSeek 等端点支持这种，但不支持 json_schema）
		resp, err = c.cli.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: shared.ChatModel(c.model),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(system + jsonRule),
				openai.UserMessage(user),
			},
			ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
				OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
			},
		})
	}
	if err != nil {
		// 降级 2：纯提示词约束，端点连 json_object 都不支持时兜底。
		// 走 ChatText（usage 在其中累计），拿到文本后统一走下面的解析。
		var text string
		text, err = c.ChatText(ctx, system+jsonRule, user)
		if err == nil {
			return unmarshalJSONContent(text, out)
		}
	}
	if err != nil {
		return fmt.Errorf("chat json: %w", err)
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("chat json: empty choices")
	}
	globalUsage.add(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	return unmarshalJSONContent(resp.Choices[0].Message.Content, out)
}

// unmarshalJSONContent 解析模型输出的 JSON；模型偶尔会在 JSON 外面包
// ```json 代码块或加几句解释，这里做一次宽容提取。
func unmarshalJSONContent(content string, out any) error {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "{"); i > 0 { // 去掉 JSON 前可能存在的解释文字或 ```json 围栏
		s = s[i:]
	}
	if strings.HasSuffix(s, "```") { // 去掉结尾围栏
		s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	}
	if err := json.Unmarshal([]byte(s), out); err != nil {
		return fmt.Errorf("parse json output: %w (raw=%.120s)", err, s)
	}
	return nil
}
