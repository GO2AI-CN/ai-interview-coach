package coach

import "fmt"

// 所有提示词集中在这里，改提示词不用翻业务代码。

// PlanSchema 是面试计划的 JSON Schema：用 Structured Outputs 锁死输出格式，
// 避免"这次给你 JSON、下次塞两句解释"的格式漂移。
var PlanSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"questions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dimension":   map[string]any{"type": "string"},
					"question":    map[string]any{"type": "string"},
					"checkpoints": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"weight":      map[string]any{"type": "integer"},
					"source":      map[string]any{"type": "string", "enum": []string{"rubric", "resume"}},
				},
				"required":             []string{"dimension", "question", "checkpoints", "weight", "source"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"questions"},
	"additionalProperties": false,
}

// EvalSchema 是回答评估的 JSON Schema。
var EvalSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"confidence": map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
		"coverage":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		"hit_points": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"missed":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"suggestion": map[string]any{"type": "string"},
		"depth_ok":   map[string]any{"type": "boolean"},
	},
	"required":             []string{"confidence", "coverage", "hit_points", "missed", "suggestion", "depth_ok"},
	"additionalProperties": false,
}

// ReportSchema 是评分报告的 JSON Schema。
var ReportSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"dimensions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string"},
					"score":   map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
					"comment": map[string]any{"type": "string"},
				},
				"required":             []string{"name", "score", "comment"},
				"additionalProperties": false,
			},
		},
		"weak_points":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"suggestions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"overall":      map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
	},
	"required":             []string{"dimensions", "weak_points", "suggestions", "overall"},
	"additionalProperties": false,
}

// planPrompt 生成面试计划。checkpoints 必须具体可验证——它们是追问判断的依据。
func planPrompt(position string, resume string) (string, string) {
	const system = `你是资深技术面试官，擅长基于岗位能力模型(rubric)设计面试题。`
	user := fmt.Sprintf(`目标岗位：%s。
输出面试计划(JSON)：5~8 题，覆盖该岗位最核心的能力维度，每题 3~5 条考察要点(checkpoints)。
要求：题目由浅入深；checkpoints 必须具体、可验证，禁止"考察基础"这类空话。
每题标注 source 字段：通用能力题为 "rubric"。`, position)
	if resume != "" {
		// 有简历时追加针对性出题指令：从简历项目里挖题
		user += fmt.Sprintf(`
另附候选人简历摘要。请额外生成 2~3 道基于简历的深挖题(source 标 "resume")：
针对简历中声称的项目经历、技术栈和量化成果提问，验证其真实性。禁止问候选人
明显不具备的技能。简历摘要：
%s`, resume)
	}
	return system, user
}

// evalPrompt 评估一轮回答：置信度 + 关键点覆盖度两把尺子。
func evalPrompt(q Question, answer string) (string, string) {
	const system = `你是严格的面试官。只依据事实评估，不许给安慰分。`
	user := fmt.Sprintf(`面试题：%s
考察要点(checkpoints)：%v
候选人回答：%s
按 JSON 输出评估：confidence(1-5，你对这个回答质量的自评)、
coverage(命中的 checkpoints 比例)、hit_points、missed、suggestion(一句话改进建议)、
depth_ok(综合判断是否已挖到位、无需再追问)。只输出 JSON。`,
		q.Question, q.Checkpoints, answer)
	return system, user
}

// reportPrompt 聚合所有评估生成最终评分报告。
func reportPrompt(position string, turns []Turn) (string, string) {
	const system = `你是资深面试官，负责输出结构化的面试评分报告。`
	user := fmt.Sprintf(`岗位：%s。以下是整场面试的题目与评估记录(JSON)：
%s
请按维度聚合打分(0-100)，列出薄弱点和可执行的改进建议，给出总分。只输出 JSON。`,
		position, turnsJSON(turns))
	return system, user
}
