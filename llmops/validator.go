package llmops

import (
	"github.com/santhosh-tekuri/jsonschema/v6"

	"ai-interview-coach/llmops/guard"
)

// validatorSchema 是 jsonschema.Schema 的别名，避免在 service.go 里散落第三方类型
type validatorSchema = jsonschema.Schema

// compileValidator 启动期编译一次 Review 的 JSON Schema 校验器
func compileValidator() (*validatorSchema, error) {
	schema, err := guard.GenerateSchema[guard.Review]()
	if err != nil {
		return nil, err
	}
	return guard.CompileValidator(schema)
}
