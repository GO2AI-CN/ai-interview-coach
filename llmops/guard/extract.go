package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"ai-interview-coach/internal/llm"
)

// CompileValidator 编译 JSON Schema 校验器（启动期一次，勿放请求热路径）
func CompileValidator(schema map[string]any) (*jsonschema.Schema, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("review.schema.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("review.schema.json")
}

// Extract 带修复循环的抽取（第 2 篇第 4 层防线）：
// 协议层由 llm.Client.ChatJSON 的 strict JSON Schema 约束完成，
// 这里做 schema 复核 + 业务规则校验，失败把错误回喂模型重试（上限 3 次）。
// Token 用量从项目进程级计数器取差值，供成本计量复用。
func Extract(ctx context.Context, client llm.Client, validator *jsonschema.Schema, systemPrompt, task string) (*Review, llm.Usage, error) {
	schema, err := GenerateSchema[Review]()
	if err != nil {
		return nil, llm.Usage{}, err
	}

	input := fmt.Sprintf(`%s

任务：%s
只输出符合契约的 JSON。`, systemPrompt, task)

	const maxAttempts = 3
	start := llm.TotalUsageSnapshot()
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var review Review
		err := client.ChatJSON(ctx, systemPrompt, input, "ticket_review", "客服工单分析结果", schema, &review)
		usage := llm.TotalUsageSnapshot().Sub(start)
		if err != nil {
			lastErr = err
			continue // 端点错误值得重试（ChatJSON 内部已做 json_schema→json_object→纯提示词降级）
		}

		// Schema 复核（对解码后的结构重新序列化再做一次校验）
		data, _ := json.Marshal(review)
		doc, jerr := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if jerr != nil {
			lastErr = fmt.Errorf("输出重新序列化失败: %w", jerr)
		} else if verr := validator.Validate(doc); verr != nil {
			lastErr = fmt.Errorf("schema 复核失败: %w", verr)
		} else if verr := review.Validate(); verr != nil {
			lastErr = verr
		} else {
			return &review, usage, nil
		}

		// 修复提示：把「错误 + 当前输出」回喂给模型
		input = fmt.Sprintf(`上一次输出未通过校验：%s
当前输出：%s
请修正后重新输出，只输出 JSON。原始任务：%s`, lastErr, data, task)
	}
	return nil, llm.TotalUsageSnapshot().Sub(start), fmt.Errorf("%d 次尝试均失败，最后错误：%w", maxAttempts, lastErr)
}

var _ = errors.Is // 保留：扩展错误分类时使用
