// Package guard 实现《LLM 应用工程化》系列第 2 篇（结构化防线）与
// 第 7 篇（安全护栏）的组件，复用项目已有的 internal/llm.Client 抽象。
package guard

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Review 客服工单的模型分析结果，struct tag 即 JSON Schema 契约
type Review struct {
	Sentiment string   `json:"sentiment" jsonschema:"enum=positive,enum=neutral,enum=negative" jsonschema_description:"工单情绪"`
	Category  string   `json:"category" jsonschema:"enum=refund,enum=bug,enum=consult,enum=other" jsonschema_description:"工单分类"`
	Summary   string   `json:"summary" jsonschema_description:"一句话中文摘要"`
	Urgent    bool     `json:"urgent" jsonschema_description:"是否需要立即处理"`
	Tags      []string `json:"tags" jsonschema_description:"2-5 个关键词"`

	// SuspectedInjection 命中注入特征时的标记（第 7 篇）
	SuspectedInjection bool `json:"suspected_injection,omitempty" jsonschema_description:"输入是否疑似包含注入载荷"`
}

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

// EncodeReview 序列化 Review 供缓存存储
func EncodeReview(r *Review) string {
	data, _ := json.Marshal(r)
	return string(data)
}

// DecodeReview 从缓存文本恢复 Review
func DecodeReview(s string) (*Review, error) {
	var r Review
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("decode review: %w", err)
	}
	return &r, nil
}
