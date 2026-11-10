// embedapi 是系列第三篇第 2 讲的实战 demo：
// 用真实 Embedding API 对第 1 讲的四条语料做语义检索，并演示磁盘缓存的效果。
// 凭证从 .env 的 EMBED_API_KEY / EMBED_BASE_URL / EMBED_MODEL 读取，与对话模型配置互相独立。
// 运行：go run ./rag/cmd/embedapi
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/joho/godotenv"

	"ai-interview-coach/rag/embedding"
)

// 语料与第 1 讲完全相同，方便对比「玩具 embedding」和「真 embedding」的检索质量。
var docs = []string{
	"使用 systemd 守护进程让面试教练 7×24 小时运行",
	"把面试历史记录保存到 SQLite 数据库，支持随时回看",
	"MCP 协议让 Agent 以标准方式调用外部工具",
	"通过 SSE 实现网页版面试的流式输出",
}

func main() {
	_ = godotenv.Load() // 从 .env 读 EMBED_API_KEY / EMBED_BASE_URL / EMBED_MODEL
	model := flag.String("model", os.Getenv("EMBED_MODEL"), "嵌入模型名（默认取 .env 的 EMBED_MODEL）")
	flag.Parse()
	if *model == "" {
		log.Fatal("未指定模型：设置 EMBED_MODEL 或用 -model 传入")
	}

	cli := embedding.New(mustEnv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"), *model, ".embed-cache")
	ctx := context.Background()

	queries := []string{"面试历史怎么保存", "怎么让程序一直跑着"}
	for _, q := range queries {
		start := time.Now()
		// 查询与语料一起嵌入：查询向量和文档向量必须来自同一个模型
		vecs, err := cli.Embed(ctx, append([]string{q}, docs...))
		if err != nil {
			log.Fatalf("embed: %v", err)
		}
		qv, docVecs := vecs[0], vecs[1:]

		type hit struct {
			score float64
			doc   string
		}
		var hits []hit
		for i, dv := range docVecs {
			s, _ := embedding.Cosine(qv, dv)
			hits = append(hits, hit{s, docs[i]})
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

		fmt.Printf("查询：%s  （%d 维, 模型 %s, 耗时 %v，第二次运行全走缓存会更快）\n",
			q, len(qv), *model, time.Since(start).Round(time.Millisecond))
		for i, h := range hits {
			fmt.Printf("  #%d  %.3f  %s\n", i+1, h.score, h.doc)
		}
	}
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("环境变量 %s 未设置（写入 .env 或 export）", k)
	}
	return v
}
