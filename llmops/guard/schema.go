package guard

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
)

// GenerateSchema 从 Go 类型生成 Structured Outputs 兼容的 JSON Schema
// （closed world，禁 additionalProperties，与官方推荐做法一致）
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
