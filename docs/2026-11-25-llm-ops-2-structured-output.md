# LLM 应用工程化②：结构化输出与 Schema 校验，给模型的 JSON 上一道保险

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。上一篇我们把 Prompt 变成了版本化资产，今天解决 LLM 应用里另一个经典的深夜报警源：**模型输出的 JSON，「差不多对了」**。

几乎每个人都写过这样的解析代码：

```go
var result map[string]interface{}
if err := json.Unmarshal([]byte(modelOutput), &result); err != nil {
    // 解析失败，重试一次
}
```

它的问题不在于不够努力，而在于方向错了：`json.Unmarshal` 只能告诉你「是不是合法 JSON」，不能告诉你「是不是**你需要的** JSON」。模型可能少一个字段、把枚举值写成同义词（"满意" vs "positive"）、给数字加个单位（"3 天" vs "3"）——每一样都是合法 JSON，每一样都会让下游悄悄翻车。

工程化的答案是**纵深防御**：不指望任何单一层级兜住所有错误，而是让四层防线各管一段。本文四层全部落地，代码核对至发布日官方最新版。

## 一、四层防线总览

```
第 1 层  协议层：Structured Outputs（让模型在协议层面只能输出合法 schema）
第 2 层  解析层：类型安全解码（json.Unmarshal 到强类型 struct）
第 3 层  校验层：JSON Schema 强校验 + 业务规则校验
第 4 层  修复层：带错误的自动重试（Repair Loop）+ 失败降级
```

层与层之间是「信任但验证」的关系：第 1 层已经很强，但官方 SDK 自己的 README 都写明一句话——

> Go's `json.Unmarshal` decodes JSON into a struct; it does not validate JSON Schema constraints or application business rules.

连 OpenAI 官方都提醒你：**解码不等于校验**。第 3、4 层不是多余，是必须。

## 二、第 1 层：Structured Outputs 协议约束

2024 年 8 月起 OpenAI 提供 Structured Outputs：服务端强制模型的输出符合给定 JSON Schema，不合规的 token 根本不会被采样出来。这比「Prompt 里求模型好好输出 JSON」可靠几个数量级。

`go.mod`：

```
module structlab

go 1.26

require (
	github.com/invopop/jsonschema v0.14.0
	github.com/openai/openai-go/v3 v3.107.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)
```

> **版本要求**：openai-go v3.107.0 要求 **Go 1.26+**（官方 README：依赖 Go 1.24+ 的 `omitzero` 序列化语义；Go 1.25 需锁 v3.74.0，更早锁 v3.44.0）。v3 起 SDK 主推 Responses API，Chat Completions 保留支持；结构化输出用 Responses API 是最顺的路径。

目标结构体——一个客服工单的模型分析结果。用 struct tag 直接声明 schema，这就是「类型即契约」：

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/invopop/jsonschema"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// Review 客服工单的模型分析结果，struct tag 即 JSON Schema 契约
type Review struct {
	Sentiment string   `json:"sentiment" jsonschema:"enum=positive,enum=neutral,enum=negative" jsonschema_description:"工单情绪"`
	Category  string   `json:"category" jsonschema:"enum=refund,enum=bug,enum=consult,enum=other" jsonschema_description:"工单分类"`
	Summary   string   `json:"summary" jsonschema_description:"一句话中文摘要"`
	Urgent    bool     `json:"urgent" jsonschema_description:"是否需要立即处理"`
	Tags      []string `json:"tags" jsonschema_description:"2-5 个关键词"`
}

// GenerateSchema 从 Go 类型生成 Structured Outputs 兼容的 schema
// （与 openai-go v3 官方 README 示例一致：closed world，禁 additionalProperties）
func GenerateSchema[T any]() (map[string]any, error) {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var v T
	schema := reflector.Reflect(v)

	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	// 保留原始 JSON，避免整数约束被 float64 舍入
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(raw))
	for k, v := range raw {
		result[k] = v
	}
	return result, nil
}
```

调用侧。v3 SDK 的结构化输出参数挂在 `Text.Format` 上，并且——**一定要处理 refusal（模型拒答）**，这是官方示例里最强调的点：

```go
// finalJSONText 从 Responses 响应中取出唯一的最终 JSON 文本
// 改编自 openai-go v3 官方 README 的 structured-outputs 示例
func finalJSONText(response *responses.Response) (*string, error) {
	if response.Status != responses.ResponseStatusCompleted {
		return nil, fmt.Errorf("response did not complete: %s", response.Status)
	}
	var text *string
	for _, item := range response.Output {
		if item.Type != "message" || (item.Phase != "" && item.Phase != responses.ResponseOutputMessagePhaseFinalAnswer) {
			continue
		}
		if item.Status != "completed" {
			return nil, errors.New("final output message did not complete")
		}
		for _, part := range item.Content {
			switch part.Type {
			case "refusal":
				return nil, errors.New("model refused the request")
			case "output_text":
				if text != nil {
					return nil, errors.New("expected one final JSON text part")
				}
				text = &part.Text
			}
		}
	}
	if text == nil {
		return nil, errors.New("response has no final JSON text")
	}
	return text, nil
}

func callModel(ctx context.Context, client *openai.Client, schema map[string]any, input string) (*string, error) {
	resp, err := client.Responses.New(ctx, responses.ResponseNewParams{
		Model: openai.ChatModelGPT5_2,
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(input),
		},
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   "ticket_review",
					Schema: schema,
					Strict: openai.Bool(true),
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return finalJSONText(resp)
}
```

## 三、第 3 层：双校验——Schema 强校验 + 业务规则

结构化输出把「schema 形状」的错误压到了接近零，但两类错误它管不了：**业务规则**（摘要超字数、标签数量越界）和**枚举值的语义漂移**（标签出现了 schema 之外的值——Strict 模式下不该发生，但防御纵深不允许我们赌）。所以校验分两半。

先编译一次校验器（启动时做一次，别放进请求热路径）：

```go
import (
	"bytes"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compileValidator(schema map[string]any) (*jsonschema.Schema, error) {
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
```

业务规则写在类型的 `Validate()` 方法里，和结构体定义放在一起，改契约时不会漏：

```go
// Validate 业务规则校验：JSON Schema 管不了的都在这里
func (r *Review) Validate() error {
	if r.Summary == "" {
		return errors.New("summary 为空")
	}
	if n := len([]rune(r.Summary)); n > 50 {
		return fmt.Errorf("summary 超过 50 字：%d", n)
	}
	if len(r.Tags) < 2 || len(r.Tags) > 5 {
		return fmt.Errorf("tags 数量 %d 不在 [2,5] 区间", len(r.Tags))
	}
	return nil
}
```

## 四、第 4 层：修复循环与降级

校验失败不等于世界末日。把**错误信息和模型刚才的输出一起喂回去**，模型通常一次就能改对——这就是 Repair Loop。但要给它画上边界：重试有上限，超限走降级。

```go
// Extract 带修复循环的完整抽取流程
func Extract(ctx context.Context, client *openai.Client, validator *jsonschema.Schema, ticket string) (*Review, error) {
	schema, err := GenerateSchema[Review]()
	if err != nil {
		return nil, err
	}

	input := fmt.Sprintf(`分析下面的客服工单，输出 JSON：
{"sentiment": "positive|neutral|negative", "category": "refund|bug|consult|other",
 "summary": "一句话中文摘要，50字以内", "urgent": true|false, "tags": ["2-5个关键词"]}

工单：%s`, ticket)

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		text, err := callModel(ctx, client, schema, input)
		if err != nil {
			// 网络错误、限流：值得退避重试；拒答则直接放弃
			if errors.Is(err, context.DeadlineExceeded) {
				lastErr = err
				continue
			}
			if err.Error() == "model refused the request" {
				return nil, err
			}
			lastErr = err
			continue
		}

		// Schema 强校验
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(*text)))
		if err != nil {
			lastErr = fmt.Errorf("输出不是合法 JSON: %w", err)
		} else if err := validator.Validate(doc); err != nil {
			lastErr = fmt.Errorf("schema 校验失败: %w", err)
		} else {
			// 类型解码 + 业务规则
			var review Review
			if err := json.Unmarshal([]byte(*text), &review); err != nil {
				lastErr = fmt.Errorf("解码失败: %w", err)
			} else if err := review.Validate(); err != nil {
				lastErr = err
			} else {
				return &review, nil
			}
		}

		// 修复提示：把「错误 + 原始输出」回喂给模型
		input = fmt.Sprintf(`上一次输出未通过校验：%s
原始输出：%s
请修正后重新输出，只输出 JSON。原始任务：%s`, lastErr, *text, ticket)
	}
	return nil, fmt.Errorf("%d 次尝试均失败，最后错误：%w", maxAttempts, lastErr)
}
```

降级的完整形态：返回一个安全的兜底值（`Category: "other"`、`Urgent: false`），同时把原始工单和错误写入死信队列，交给人工或离线任务处理。**绝不让解析错误向上游传播成 500**。`main` 串起整个流程：

```go
func main() {
	ctx := context.Background()
	client := openai.NewClient() // 默认读 OPENAI_API_KEY 环境变量

	schema, err := GenerateSchema[Review]()
	if err != nil {
		log.Fatal(err)
	}
	validator, err := compileValidator(schema)
	if err != nil {
		log.Fatal(err)
	}

	review, err := Extract(ctx, client, validator,
		"客户：升级到 v3 之后导入直接 panic，报一堆错，你们什么时候能修？不能修我们就退款！")
	if err != nil {
		log.Printf("抽取失败，走降级：%v", err)
		review = &Review{Category: "other", Sentiment: "neutral", Summary: "（待人工处理）", Tags: []string{"pending"}}
	}
	fmt.Printf("%+v\n", *review)
}
```

> **模型选型提示**：`openai.ChatModelGPT5_2` 沿用官方 README 示例；Structured Outputs 要求较新的模型（GPT-4o 系列及以上），老模型会收到参数错误。换模型时确认它在[官方支持列表](https://platform.openai.com/docs/guides/structured-outputs)内。

## 五、踩坑清单

1. **解码 ≠ 校验**：`json.Unmarshal` 成功只能说明语法合法。schema 校验和业务校验缺一不可，这是官方 README 明示的边界。
2. **refusal 必须处理**：内容审核触发拒答时响应里**没有 JSON**。不处理 refusal，你的错误日志里会充满莫名其妙的「EOF」。
3. **Strict 模式的 schema 限制**：Structured Outputs 只支持 JSON Schema 子集，`additionalProperties: false` 是必选项——所以官方示例的 Reflector 才设置 `AllowAdditionalProperties: false`。手写 schema 时漏掉它会被服务端拒绝。
4. **校验器只编译一次**：`compileValidator` 放在启动流程，请求热路径里只做 `Validate`。
5. **重试要分类**：网络错误/限流（429、5xx）退避重试；拒答、参数错误重试无意义，直接失败或降级。
6. **修复提示里带上原始输出**：只告诉模型「你错了」它不知道自己错哪；把它的原始输出回贴，修复成功率会高得多。
7. **枚举值写死在 schema 里**：用 `jsonschema:"enum=a,enum=b"` 而不是 Prompt 里描述取值范围——前者由协议强制，后者靠模型自觉。

## 六、本篇小结与下篇预告

四层防线各就各位：**协议层**消灭格式错误，**校验层**兜住语义错误，**修复层**把偶发失败收敛到接近零。到这一步，单次调用已经够稳了——但「稳」和「好」是两回事。下一篇回答：**怎么证明你的 Prompt 改动、模型升级没有让效果变差？** 我们搭一个自动化评测集，把「感觉变好了」变成 CI 里的一行数字。

---

## 版本说明（2026-11-25 核对）

| 依赖 | 本文使用版本 | 核对来源 |
| --- | --- | --- |
| Go | 1.26+（openai-go v3.107.0 硬性要求） | 官方 README Requirements |
| github.com/openai/openai-go/v3 | v3.107.0（2026-11-24） | proxy.golang.org `@latest` |
| github.com/invopop/jsonschema | v0.14.0 | proxy.golang.org `@latest` |
| github.com/santhosh-tekuri/jsonschema/v6 | v6.0.3 | proxy.golang.org `@latest` |

Structured Outputs 的 API 形态（`Responses.New` + `Text.Format.OfJSONSchema` + `GenerateSchema`）取自 openai-go v3.107.0 官方 README 示例，逐字段核对。

## 相关阅读

- [LLM 应用工程化①：Prompt 管理别再硬编码，版本化、灰度发布与一键回滚](https://blog.911015.com/ai/llm-ops-1-prompt-mgmt.html) —— 第 3 篇评测集按版本对比指标时，会用到第 1 篇的决策日志
- [LLM 应用工程化系列聚合目录](https://blog.911015.com/ai/llm-ops.html)
- [Go AI 应用开发系列08 - 结构化输出：Go 泛型 + JSON Schema 的类型安全之道](https://blog.911015.com/ai/go-ai-08-structured-output.html) —— Go 侧类型安全设计的深入展开
- [Go MCP 实战5 - MCP 函数调用循环：让模型自己决定调工具](https://blog.911015.com/ai/go-mcp-5-client-loop.html) —— 函数调用是结构化输出的另一种协议形态

> **声明**：本文由 AI 辅助生成，代码均对照官方文档与源码核验；如发现任何错误或不一致，欢迎在评论区指正。
