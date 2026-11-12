// chat 是系列第三篇第 5 讲的完整问答链路 CLI：
// 每轮输入 → bge-m3 嵌入问题 → 向量库 top-k 检索 → 拼带引用标号的 prompt →
// OpenAI 兼容端点（DeepSeek/Kimi 等）SSE 流式生成 → 末尾打印引用来源清单。
//
// 多轮对话采用朴素 RAG 方案：历史消息原样回传，每轮新问题独立检索。
//
// 运行（仓库根目录，需先 go run ./rag/cmd/ingest 建库）：
//
//	go run ./rag/cmd/chat
//	go run ./rag/cmd/chat -k 5
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/prompt"
	"ai-interview-coach/rag/store"
)

func main() {
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	topK := flag.Int("k", 3, "每轮检索命中块数")
	flag.Parse()
	_ = godotenv.Load()

	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	ctx := context.Background()
	if n := st.Count(ctx); n <= 0 {
		log.Fatal("向量库为空：先跑 go run ./rag/cmd/ingest")
	}
	fmt.Printf("chat: 库内 %d 块，输入问题开始对话（Ctrl+C 退出）\n", st.Count(ctx))

	emb := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), ".embed-cache")
	model := os.Getenv("COACH_MODEL")
	if model == "" {
		model = "gpt-4o-mini" // 兼容端点未配置模型时的兜底
	}
	cli := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithBaseURL(os.Getenv("OPENAI_BASE_URL")),
	)

	history := []openai.ChatCompletionMessageParamUnion{} // 只存问答对，不存检索资料
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n你> ")
		if !scanner.Scan() {
			break
		}
		q := strings.TrimSpace(scanner.Text())
		if q == "" {
			continue
		}

		// 1. 检索：新问题独立嵌入（缓存命中不花钱）→ top-k 命中块
		qvecs, err := emb.Embed(ctx, []string{q})
		if err != nil {
			log.Printf("嵌入问题失败: %v", err)
			continue
		}
		hits, err := st.Search(ctx, qvecs[0], *topK, nil)
		if err != nil {
			log.Printf("检索失败: %v", err)
			continue
		}
		system, user := prompt.Build(q, hits)

		// 2. 组装消息：system 每轮重建（内含本轮资料），历史只跟问答对
		msgs := append([]openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system)}, history...)
		msgs = append(msgs, openai.UserMessage(user))

		// 3. SSE 流式生成：逐 token 上屏，首 token 到达即体感"开始回答"
		stream := cli.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(model),
			Messages: msgs,
		})
		fmt.Print("\nAI> ")
		var answer strings.Builder
		for stream.Next() {
			delta := stream.Current().Choices[0].Delta.Content
			answer.WriteString(delta)
			fmt.Print(delta) // 边收边打印，不等全文
		}
		if err := stream.Err(); err != nil {
			log.Printf("生成失败: %v", err)
			continue
		}
		_ = stream.Close()

		// 4. 回答入库 + 引用来源清单，读者可凭 [n] 回查原文小节
		history = append(history, openai.UserMessage(q), openai.AssistantMessage(answer.String()))
		fmt.Println("\n\n—— 引用来源 ——")
		for _, s := range prompt.Sources(hits) {
			fmt.Println(s)
		}
	}
}
