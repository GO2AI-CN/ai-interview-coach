// eval 是系列第 6 篇《检索质量进阶》的评测 CLI：
// 内置 10 道「问题 → 期望来源文件」考题（含前两篇实测翻车的两道题），
// 分别跑纯向量 / 纯 BM25 / RRF 混合三路检索，统计 top-k 命中率（k=1,3,5），
// 用真实数据回答「top-k 取多大、混合检索值不值」。
//
// 评测方法：对每道题取向量库全量块（nResults=块总数）的完整向量排名；
// 同一批块建 BM25 倒排索引做关键词排名；两路排名用 RRF 融合。
// 命中判定：top-k 内任意块的来源文件等于期望文件即记 1 分。
//
// 运行（仓库根目录，需先 go run ./rag/cmd/ingest 建库）：
//
//	go run ./rag/cmd/eval        # 汇总表
//	go run ./rag/cmd/eval -v     # 逐题打印三路 top-5 明细
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"ai-interview-coach/rag/chunk"
	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/hybrid"
	"ai-interview-coach/rag/store"
)

// exam 是一道评测题：问题 + 期望命中的来源文件。
type exam struct {
	q    string
	want string // 期望来源文件名（ingest 时的 file 元数据）
	note string // 备注，标记已知的翻车题
}

// exams 覆盖库里两个系列的常见问法；note 标「翻车」的是第 4/5 篇实测跑偏的案例。
var exams = []exam{
	{"面试教练怎么判断回答太浅、决定追问？", "2026-10-27-ai-interview-coach-1.md", ""},
	{"MCP server 怎么通过 HTTP 对外提供服务？", "2026-11-04-go-mcp-7-remote-http.md", "翻车：第 4 篇 top-5 全是概念章"},
	{"MCP 的 Sampling 是干什么的？", "2026-11-02-go-mcp-6-prompts-sampling-roots.md", ""},
	{"systemd 怎么让 Go 服务 7×24 运行？", "2026-10-29-ai-interview-coach-6-deploy.md", ""},
	{"飞书机器人怎么接入，需要公网 IP 吗？", "2026-10-28-ai-interview-coach-4-feishu.md", ""},
	{"面试历史怎么落库 SQLite？", "2026-10-31-ai-interview-coach-7-sqlite.md", ""},
	{"怎么把面试历史库包装成 MCP 服务？", "2026-11-02-go-mcp-4-coachstore-server.md", ""},
	{"MCP 的 Tools 和 Resources 有什么区别？", "2026-11-02-go-mcp-3-tools-resources.md", ""},
	{"语音作答的 ASR 方案怎么选？", "2026-10-28-ai-interview-coach-5-voice.md", ""},
	{"怎么把面试教练改造成别的 Agent？", "2026-10-31-ai-interview-coach-8-finale.md", ""},
}

var ks = []int{1, 3, 5} // 统计口径：top-k 内命中期望文件即得分

// ranks 是一道题的三路排名：向量 / BM25 / RRF 混合。
type ranks struct {
	vec, bm25, rrf []hybrid.Scored
}

func main() {
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	verbose := flag.Bool("v", false, "逐题打印三路检索明细")
	flag.Parse()
	_ = godotenv.Load()

	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	ctx := context.Background()
	n := st.Count(ctx)
	if n <= 0 {
		log.Fatal("向量库为空：先跑 go run ./rag/cmd/ingest")
	}
	fmt.Printf("eval: 库内 %d 块，%d 道题，三路检索对比（命中=期望文件进入 top-k）\n\n", n, len(exams))

	emb := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), ".embed-cache")

	// 三路排名：向量 / BM25 / RRF 混合（每路都是完整排名，k 取多少都够）
	all := make([]ranks, len(exams))
	var bm *hybrid.BM25

	for i, e := range exams {
		qvecs, err := emb.Embed(ctx, []string{e.q})
		if err != nil {
			log.Fatalf("嵌入问题失败: %v", err)
		}
		// 一次取回全量块：既是向量的完整排名，也是 BM25 的语料（保证两路同库）
		full, err := st.Search(ctx, qvecs[0], n, nil)
		if err != nil {
			log.Fatalf("检索失败: %v", err)
		}
		vec := make([]hybrid.Scored, len(full))
		for j, h := range full {
			vec[j] = hybrid.Scored{Chunk: h.Chunk, Score: float64(h.Score)}
		}
		if bm == nil { // 用第一批全量块建一次 BM25 索引，后面各题复用
			bm = hybrid.NewBM25(chunksOf(vec))
		}
		kw := bm.Search(e.q)
		all[i] = ranks{vec: vec, bm25: kw, rrf: hybrid.RRF(60, vec, kw)}
		if *verbose {
			dump(e, all[i])
		}
	}

	// 汇总：每路在每个 k 上的命中题数
	fmt.Println("方法        hit@1  hit@3  hit@5")
	report := func(name string, pick func(ranks) []hybrid.Scored) {
		var cells [3]int
		for i, e := range exams {
			r := pick(all[i])
			for j, k := range ks {
				if hit(r, e.want, k) {
					cells[j]++
				}
			}
		}
		fmt.Printf("%-10s  %d/%d   %d/%d   %d/%d\n", name,
			cells[0], len(exams), cells[1], len(exams), cells[2], len(exams))
	}
	report("纯向量", func(r ranks) []hybrid.Scored { return r.vec })
	report("纯BM25", func(r ranks) []hybrid.Scored { return r.bm25 })
	report("RRF混合", func(r ranks) []hybrid.Scored { return r.rrf })
}

// chunksOf 从检索结果里还原 chunk 列表（建 BM25 语料用）。
func chunksOf(ss []hybrid.Scored) []chunk.Chunk {
	out := make([]chunk.Chunk, len(ss))
	for i, s := range ss {
		out[i] = s.Chunk
	}
	return out
}

// hit 判断排名前 k 内是否命中期望文件。
func hit(r []hybrid.Scored, want string, k int) bool {
	return rankOf(r, want) >= 1 && rankOf(r, want) <= k
}

// rankOf 返回期望文件在排名中的首个名次（1 起），未出现返回 0。
func rankOf(r []hybrid.Scored, want string) int {
	for i, s := range r {
		if s.Chunk.File == want {
			return i + 1
		}
	}
	return 0
}

// dump 逐题打印三路 top-5 明细与正主名次（-v 模式），期望文件用 ★ 标出。
func dump(e exam, r ranks) {
	fmt.Printf("Q: %s（期望 %s）%s\n", e.q, e.want, e.note)
	fmt.Printf("  正主名次: 向量 #%d / BM25 #%d / RRF #%d\n",
		rankOf(r.vec, e.want), rankOf(r.bm25, e.want), rankOf(r.rrf, e.want))
	show := func(name string, ss []hybrid.Scored) {
		fmt.Printf("  %-6s ", name)
		for i := 0; i < 5 && i < len(ss); i++ {
			mark := "  "
			if ss[i].Chunk.File == e.want {
				mark = "★ "
			}
			head := []rune(ss[i].Chunk.Head)
			if len(head) > 10 {
				head = head[:10]
			}
			fmt.Printf("%s%d.%s§%s", mark, i+1, shortFile(ss[i].Chunk.File), string(head))
		}
		fmt.Println()
	}
	show("向量", r.vec)
	show("BM25", r.bm25)
	show("RRF", r.rrf)
	fmt.Println()
}

// shortFile 把长文件名压成可读的短形式（去日期前缀和 .md 扩展名）。
func shortFile(f string) string {
	if len(f) > 11 && f[4] == '-' && f[7] == '-' {
		f = f[11:]
	}
	return strings.TrimSuffix(f, ".md")
}
