// sync 是系列第 7 篇《生产化》的增量索引 CLI，取代全量 ingest 成为日常维护命令。
//
// 它给每个已索引文件记账（向量库目录下的 manifest.json：内容哈希 + 块 ID 清单），
// 每次运行对比磁盘与账本：
//   - 新增/变更的文件 → 先按旧账删残块，再重新切分嵌入写回；
//     同文件未改动的块命中 embedding 磁盘缓存，重切也不花钱；
//   - 磁盘上消失的文件 → 按旧账把它的块从向量库清掉；
//   - 哈希一致的文件 → 整块跳过，零嵌入零写入。
//
// 运行（仓库根目录，凭证从 .env 的 EMBED_* 读取）：
//
//	go run ./rag/cmd/sync                        # 同步全部 docs/*.md
//	go run ./rag/cmd/sync -glob "docs/2026-11-*.md"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"

	"ai-interview-coach/rag/chunk"
	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/index"
	"ai-interview-coach/rag/store"
)

func main() {
	glob := flag.String("glob", "docs/*.md", "待同步文章的模式")
	dbPath := flag.String("db", ".vecdb", "向量库目录（账本 manifest.json 也存这里）")
	cache := flag.String("cache", ".embed-cache", "embedding 磁盘缓存目录")
	flag.Parse()
	_ = godotenv.Load()
	start := time.Now()

	// 1. 盘点磁盘现状：文件内容 → 哈希
	files, err := filepath.Glob(*glob)
	if err != nil || len(files) == 0 {
		log.Fatalf("glob %q 没有匹配到文件", *glob)
	}
	disk := map[string]string{}
	contents := map[string][]byte{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("读取 %s: %v", f, err)
		}
		base := filepath.Base(f)
		disk[base] = index.Hash(data)
		contents[base] = data
	}

	// 2. 对比账本，把文件分成 重建/删除/未变 三类
	m, err := index.Load(index.Path(*dbPath))
	if err != nil {
		log.Fatalf("读取账本: %v", err)
	}
	rebuild, deleted, unchanged := m.Plan(disk)
	fmt.Printf("sync: 匹配 %d 个文件——未变 %d，新增/变更 %d，已删除 %d\n",
		len(disk), len(unchanged), len(rebuild), len(deleted))

	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	ctx := context.Background()

	// 3. 清掉已消失文件的残块（按旧账逐 ID 删，删得干净）
	for _, f := range deleted {
		if err := st.Delete(ctx, m.Files[f].IDs...); err != nil {
			log.Fatalf("删除 %s 的残块: %v", f, err)
		}
		delete(m.Files, f)
	}

	// 4. 重建新增/变更文件：先删旧块（块数变少不留残块），再切分收集
	var chunks []chunk.Chunk
	newIDs := map[string][]string{}
	for _, f := range rebuild {
		if old, ok := m.Files[f]; ok {
			if err := st.Delete(ctx, old.IDs...); err != nil {
				log.Fatalf("删除 %s 的旧块: %v", f, err)
			}
		}
		cs := chunk.TagSeries(chunk.TagFile(f, chunk.ByHeading(string(contents[f]), 300)))
		newIDs[f] = index.IDsFor(f, len(cs))
		chunks = append(chunks, cs...)
	}

	// 5. 嵌入并写回：缓存命中即返回，只有真正新写的块才调 API
	if len(chunks) > 0 {
		emb := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
			os.Getenv("EMBED_MODEL"), *cache)
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}
		vectors, stats, err := emb.EmbedWithStats(ctx, texts)
		if err != nil {
			log.Fatalf("嵌入失败: %v", err)
		}
		if err := st.AddChunks(ctx, chunks, vectors); err != nil {
			log.Fatalf("写入向量库: %v", err)
		}
		fmt.Printf("sync: 重建 %d 块（缓存命中 %d，回源 API %d）\n", len(chunks), stats.Hit, stats.Miss)
		for f := range newIDs {
			m.Files[f] = index.FileState{Hash: disk[f], IDs: newIDs[f]}
		}
	}

	// 6. 账本落盘，收工
	if err := m.Save(index.Path(*dbPath)); err != nil {
		log.Fatalf("保存账本: %v", err)
	}
	fmt.Printf("sync: 完成，库内共 %d 块，耗时 %.1fs\n", st.Count(ctx), time.Since(start).Seconds())
}
