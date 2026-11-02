package main

import (
	"context"
	"encoding/json"
	"regexp"
)

// scriptedLLM 是一台不联网的「假模型」，行为完全确定：
// 第一轮请求 list_sessions，第二轮从工具结果里抓场次 ID 请求 get_report，
// 第三轮把报告内容总结为最终答复。
// main 在没有 API key 时用它演示全链路；e2e 测试也用它做可断言的端到端。
type scriptedLLM struct {
	step int
}

var sessionIDRe = regexp.MustCompile(`#(\d+)`)

func (l *scriptedLLM) Chat(_ context.Context, messages []Message, _ []Tool) (*Message, error) {
	l.step++
	switch l.step {
	case 1:
		return &Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID:        "call_1",
			Name:      "list_sessions",
			Arguments: json.RawMessage(`{}`),
		}}}, nil
	case 2:
		id := ""
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "tool" {
				if m := sessionIDRe.FindStringSubmatch(messages[i].Text); m != nil {
					id = m[1]
				}
				break
			}
		}
		if id == "" {
			return &Message{Role: "assistant", Text: "历史库里还没有任何面试记录"}, nil
		}
		return &Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID:        "call_2",
			Name:      "get_report",
			Arguments: json.RawMessage(`{"id":` + id + `}`),
		}}}, nil
	default:
		// 最后一条 tool 消息就是报告全文，「总结」后收工
		report := ""
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "tool" {
				report = messages[i].Text
				break
			}
		}
		return &Message{Role: "assistant",
			Text: "已为你取回最近一场面试的报告：\n" + report}, nil
	}
}
