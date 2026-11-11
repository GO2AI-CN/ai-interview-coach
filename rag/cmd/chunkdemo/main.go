// chunkdemo 是系列第三篇第 3 讲《文档切分实战》的对比实测：
// 用三种策略切分本仓库 16 篇已发布教程，对同一组问题分别检索，看命中质量的差异。
// 运行：go run ./rag/cmd/chunkdemo（凭证读 .env 的 EMBED_*，嵌入结果走 .embed-cache）
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/joho/godotenv"

	"ai-interview-coach/rag/chunk"
	"ai-interview-coach/rag/embedding"
)

// 两个测试问题：答案分别藏在面试教练第 1 篇和 MCP 第 7 篇的某个小节里。
var queries = []string{
	"面试教练怎么判断回答太浅、决定追问",
	"MCP server 怎么通过 HTTP 对外提供服务",
}

// strategies 三种切分策略：同一批文档、同一组问题，只换切法。
var strategies = []struct {
	name string
	fn   func(string) []chunk.Chunk
}{
	{"固定窗口(300字/重叠50)", func(t string) []chunk.Chunk { return chunk.BySize(t, 300, 50) }},
	{"段落合并(上限300字)", func(t string) []chunk.Chunk { return chunk.ByParagraph(t, 300) }},
	{"标题小节(超长内部再切)", func(t string) []chunk.Chunk { return chunk.ByHeading(t, 300) }},
}

func main() {
	_ = godotenv.Load()
	cli := embedding.New(mustEnv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"), mustEnv("EMBED_MODEL"), ".embed-cache")
	ctx := context.Background()

	files, _ := filepath.Glob("docs/2026-10-*.md") // 16 篇已发布教程
	if len(files) == 0 {
		log.Fatal("docs/2026-10-*.md 未找到，请在仓库根目录运行")
	}

	for _, st := range strategies {
		// 1) 切分全部文档并打元数据
		var cs []chunk.Chunk
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				log.Fatal(err)
			}
			cs = append(cs, chunk.TagFile(f, st.fn(string(b)))...)
		}
		// 2) 批量嵌入（缓存命中的不再请求 API）
		texts := make([]string, len(cs))
		for i, c := range cs {
			texts[i] = c.Text
		}
		vecs, err := cli.Embed(ctx, texts)
		if err != nil {
			log.Fatalf("embed: %v", err)
		}
		total := 0
		for _, t := range texts {
			total += len([]rune(t))
		}
		fmt.Printf("\n===== %s | %d 块, 平均 %d 字 =====\n", st.name, len(cs), total/len(cs))

		// 3) 每个问题取 top-2，看命中的块出自哪篇哪节
		for _, q := range queries {
			qv, err := cli.Embed(ctx, []string{q})
			if err != nil {
				log.Fatal(err)
			}
			type hit struct {
				score float64
				c     chunk.Chunk
			}
			var hits []hit
			for i, v := range vecs {
				s, _ := embedding.Cosine(qv[0], v)
				hits = append(hits, hit{s, cs[i]})
			}
			sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
			fmt.Printf("  查询：%s\n", q)
			for i := 0; i < 2; i++ {
				h := hits[i]
				r := []rune(h.c.Text)
				if len(r) > 40 {
					r = r[:40]
				}
				fmt.Printf("    #%d %.3f [%s] {%s} %s…\n", i+1, h.score, h.c.File, h.c.Head, string(r))
			}
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
