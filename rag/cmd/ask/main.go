// ask 是系列第三篇第 4 讲的检索验证 CLI：
// 加载 ingest 建好的向量库，对提问做 top-k 语义检索，打印命中块的来源与相似度。
// 注意这里只到「检索」为止——把命中块拼进 prompt 交给大模型生成答案，
// 是第 5 讲《完整问答链路》的内容。
//
// 运行（仓库根目录）：
//
//	go run ./rag/cmd/ask "MCP 服务怎么发布到注册表？"
//	go run ./rag/cmd/ask -k 5 "怎么给 Agent 加工具？"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"

	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/store"
)

func main() {
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	topK := flag.Int("k", 3, "返回命中块数")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("用法：go run ./rag/cmd/ask [-k N] \"你的问题\"")
	}
	question := flag.Arg(0)
	_ = godotenv.Load()

	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	ctx := context.Background()
	n := st.Count(ctx)
	if n <= 0 {
		log.Fatalf("向量库为空：先跑 go run ./rag/cmd/ingest")
	}
	fmt.Printf("ask: 库内 %d 块，问题：%s\n\n", n, question)

	// 查询向量和文档向量必须同模型——这里沿用 ingest 的 bge-m3，缓存同样生效
	cli := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), ".embed-cache")
	qvecs, err := cli.Embed(ctx, []string{question})
	if err != nil {
		log.Fatalf("嵌入问题失败: %v", err)
	}

	hits, err := st.Search(ctx, qvecs[0], *topK, nil)
	if err != nil {
		log.Fatalf("检索失败: %v", err)
	}
	for i, h := range hits {
		snippet := []rune(h.Chunk.Text)
		if len(snippet) > 120 { // 控制台只预览开头，全文留给第 5 讲的 prompt 拼接
			snippet = snippet[:120]
		}
		fmt.Printf("【%d】%.3f  %s § %s\n%s…\n\n",
			i+1, h.Score, h.Chunk.File, h.Chunk.Head, string(snippet))
	}
}
