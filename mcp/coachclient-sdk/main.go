// mcp/coachclient-sdk —— 官方 SDK 版 client + 函数调用循环演示。
//
// 用法与手写版 mcp/coachclient 完全一致（在仓库根目录）：
//
//	COACH_DB_PATH=./coach.db go run ./mcp/coachclient-sdk            # 无 key：假模型演示全链路
//	COACH_DB_PATH=./coach.db COACH_MODEL=xxx go run ./mcp/coachclient-sdk "我哪类题丢分最多？"
//
//	COACHSTORE_BIN=/绝对路径/coachstore.exe 可改用编译好的 server（默认 go run ./mcp/coachstore）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

const systemPrompt = `你是面试历史分析助手，通过 MCP 工具查询用户的模拟面试历史。
规则：先用 list_sessions 找到场次，再按需调用 get_session 或 get_report；
工具报错时阅读错误信息自行修正，不要编造数据。`

func main() {
	ctx := context.Background()

	bin := os.Getenv("COACHSTORE_BIN")
	var args []string
	if bin == "" {
		bin = "go"
		args = []string{"run", "./mcp/coachstore"}
	}

	client, err := NewClient(ctx, bin, args...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接 MCP server 失败:", err)
		os.Exit(1)
	}
	defer client.Close()

	tools, err := client.ListTools(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "拉取工具清单失败:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[client] server 提供 %d 个工具:", len(tools))
	for _, t := range tools {
		fmt.Fprintf(os.Stderr, " %s", t.Name)
	}
	fmt.Fprintln(os.Stderr)

	// 真模型优先，无 key 时用假模型演示同样的循环
	var llm LLM = &scriptedLLM{}
	if os.Getenv("OPENAI_API_KEY") != "" {
		llm = NewOpenAILLM(os.Getenv("OPENAI_BASE_URL"), os.Getenv("OPENAI_API_KEY"),
			os.Getenv("COACH_MODEL"))
		fmt.Fprintln(os.Stderr, "[client] 使用真实模型:", os.Getenv("COACH_MODEL"))
	} else {
		fmt.Fprintln(os.Stderr, "[client] 未检测到 OPENAI_API_KEY，使用内置演示模型")
	}

	question := "我最近的面试情况怎么样？"
	if len(os.Args) > 1 {
		question = os.Args[1]
	}

	// exec 桥接循环与 client：循环只认 Executor 函数签名，不知道 MCP 的存在
	answer, err := RunLoop(ctx, llm,
		func(ctx context.Context, name string, args json.RawMessage) (string, error) {
			return client.CallTool(ctx, name, args)
		},
		systemPrompt, question, tools, 8)
	if err != nil {
		fmt.Fprintln(os.Stderr, "循环失败:", err)
		os.Exit(1)
	}
	fmt.Println(answer)
}
