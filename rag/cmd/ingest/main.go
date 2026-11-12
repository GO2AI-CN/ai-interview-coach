// ingest 是系列第三篇第 4 讲的索引构建 CLI：
// docs/2026-10-*.md（16 篇已发布教程）→ 标题小节切分 → bge-m3 批量嵌入 →
// 写入 chromem-go 持久化向量库。重复运行幂等：同 ID 块覆盖更新，缓存命中不重复计费。
//
// 运行（仓库根目录，凭证从 .env 的 EMBED_* 读取）：
//
//	go run ./rag/cmd/ingest                  # 索引 16 篇文章，库落 .vecdb/
//	go run ./rag/cmd/ingest -glob "docs/2026-11-*.md"   # 增量追加新文章
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"

	"ai-interview-coach/rag/chunk"
	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/store"
)

func main() {
	glob := flag.String("glob", "docs/2026-10-*.md", "待索引文章的模式")
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	flag.Parse()
	_ = godotenv.Load()

	files, err := filepath.Glob(*glob)
	if err != nil || len(files) == 0 {
		log.Fatalf("glob %q 没有匹配到文件", *glob)
	}
	fmt.Printf("ingest: 匹配 %d 个文件\n", len(files))

	// 1. 切分：沿用第 3 篇实测最优的标题小节策略
	var chunks []chunk.Chunk
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("读取 %s: %v", f, err)
		}
		cs := chunk.ByHeading(string(data), 300)
		for i := range cs { // 补上文件来源，第 3 篇 demo 里这步在调用侧做
			cs[i].File = filepath.Base(f)
		}
		chunks = append(chunks, cs...)
	}
	fmt.Printf("ingest: 切出 %d 块\n", len(chunks))

	// 2. 嵌入：磁盘缓存命中即返回，只有新块才调 API
	cli := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), ".embed-cache")
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vectors, err := cli.Embed(context.Background(), texts)
	if err != nil {
		log.Fatalf("嵌入失败: %v", err)
	}

	// 3. 写入持久化向量库
	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	ctx := context.Background()
	if err := st.AddChunks(ctx, chunks, vectors); err != nil {
		log.Fatalf("写入向量库: %v", err)
	}
	fmt.Printf("ingest: 完成，库内共 %d 块（目录 %s/）\n", st.Count(ctx), *dbPath)
}
