// Package prompt 是 RAG 系列的检索结果组装层（第 5 篇配套代码）：
// 把 top-k 命中块拼进 prompt，并给每条资料编号，让模型的回答可以溯源。
//
// 设计要点：
//   - 资料以 [n] 编号注入，system 指令要求模型在句末标注 [n] 出处、
//     资料未覆盖就明说"知识库中没有相关信息"——回答可溯源、不编造，
//     是 RAG 问答区别于普通 chatbot 的根本；
//   - 返回 (system, user) 两个字符串而不是消息切片：prompt 包不依赖
//     任何 SDK 类型，换模型/换 SDK 时这层零改动；
//   - Sources 输出 [n] → 文件小节的映射，回答打印完后展示给读者核对。
package prompt

import (
	"fmt"
	"strings"

	"ai-interview-coach/rag/store"
)

// Build 把问题和命中块组装成 system + user 两条消息的内容。
func Build(question string, hits []store.Result) (system, user string) {
	var b strings.Builder
	b.WriteString("你是「Go2AI之路」知识库问答助手。请只依据下面编号的资料回答，" +
		"在相关句末用 [n] 标注出处编号；资料没有覆盖的内容直接说明\"知识库中没有相关信息\"，不要编造。\n\n资料：\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] 《%s》§%s（相关度 %.2f）\n%s\n\n",
			i+1, h.Chunk.File, h.Chunk.Head, h.Score, h.Chunk.Text)
	}
	return b.String(), "问题：" + question
}

// Sources 返回引用清单，把回答里的 [n] 映射回 文件§小节 + 相关度。
func Sources(hits []store.Result) []string {
	srcs := make([]string, len(hits))
	for i, h := range hits {
		srcs[i] = fmt.Sprintf("[%d] 《%s》§%s（相关度 %.2f）",
			i+1, h.Chunk.File, h.Chunk.Head, h.Score)
	}
	return srcs
}
