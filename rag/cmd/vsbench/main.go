// vsbench 是系列第 8 篇（收官·向量库横评）的配套压测 CLI，回答两个问题：
//
//	1) 我们手上的真实库多快？——打开持久化 .vecdb 计时、统计落盘体积、
//	   用第 6 篇的 10 道考题压 top-3 查询延迟（嵌入全走磁盘缓存，零 API 成本）；
//	2) chromem-go 的边界在哪？——以真实 357 块向量为种子加噪合成
//	   5k / 20k / 50k 块，分别测建库耗时、查询延迟分布与堆内存，
//	   并对照第 1 篇那种单线程暴力余弦，看嵌入式库到底买了什么。
//
// 合成方法：v' = normalize(seed + ε·g)，g 为固定种子的标准正态向量，
// ε=0.05——扰动很小，合成块仍贴在真实语义的流形附近，规模效应才可信。
//
// 运行（仓库根目录，需 .env 里的 EMBED_* 配置）：
//
//	go run ./rag/cmd/vsbench                  # 全量（含 50k 规模档）
//	go run ./rag/cmd/vsbench -upto 5000       # 只跑到指定规模
//	go run ./rag/cmd/vsbench -q 50            # 每档查询次数
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/joho/godotenv"
	"github.com/philippgille/chromem-go"

	"ai-interview-coach/rag/chunk"
	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/store"
)

// questions 复用第 6 篇评测的 10 道考题（只取问题，这里只测延迟不判命中）。
var questions = []string{
	"面试教练怎么判断回答太浅、决定追问？",
	"MCP server 怎么通过 HTTP 对外提供服务？",
	"MCP 的 Sampling 是干什么的？",
	"systemd 怎么让 Go 服务 7×24 运行？",
	"飞书机器人怎么接入，需要公网 IP 吗？",
	"面试历史怎么落库 SQLite？",
	"怎么把面试历史库包装成 MCP 服务？",
	"MCP 的 Tools 和 Resources 有什么区别？",
	"语音作答的 ASR 方案怎么选？",
	"怎么把面试教练改造成别的 Agent？",
}

// scales 是规模压测的档位；realScale 档占位 0，运行时替换为真实种子块数（纯种子，不加噪）。
var scales = []int{0, 5_000, 20_000, 50_000}

func main() {
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	upto := flag.Int("upto", 50_000, "规模压测最大档")
	nq := flag.Int("q", 30, "规模压测每档查询次数")
	flag.Parse()
	_ = godotenv.Load()
	ctx := context.Background()

	emb := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), ".embed-cache")

	// ---------- 第一部分：真实库体检 ----------
	fmt.Printf("== 真实库体检（%s，%s，chromem-go 版本见 go.mod）==\n", *dbPath, runtime.Version())
	t0 := time.Now()
	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	load := time.Since(t0)
	n := st.Count(ctx)
	if n <= 0 {
		log.Fatal("向量库为空：先跑 go run ./rag/cmd/ingest")
	}
	fmt.Printf("块数 %d，落盘 %s，打开（含加载）耗时 %s\n", n, dirSize(*dbPath), load)

	qvecs, err := emb.Embed(ctx, questions)
	if err != nil {
		log.Fatalf("嵌入问题失败: %v", err)
	}
	var durs []time.Duration
	for _, qv := range qvecs { // 亚毫秒级操作批量计时：每题查 5 遍取平均
		t := time.Now()
		for r := 0; r < 5; r++ {
			if _, err := st.Search(ctx, qv, 3, nil); err != nil {
				log.Fatalf("检索失败: %v", err)
			}
		}
		durs = append(durs, time.Since(t)/5)
	}
	fmt.Printf("top-3 查询（10 题 × 5 遍）：%s\n\n", stat(durs))

	// ---------- 第二部分：规模压测 ----------
	seeds, dim := seedVectors(ctx, emb)
	fmt.Printf("== 规模压测（种子 %d 块 × %d 维，ε=0.05，每档 %d 次查询）==\n", len(seeds), dim, *nq)
	fmt.Println("规模\t建库耗时\ttop-3 平均\ttop-3 最大\t暴力平均\t堆内存增量")
	for _, N := range scales {
		if N == 0 {
			N = len(seeds) // 第一档：真实规模，纯种子
		}
		if N > *upto {
			continue
		}
		addNoise := N != len(seeds) // 真实档不加噪，保持原向量
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before) // 快照含合成向量：统计这一规模进程总共多吃多少
		vecs := synth(seeds, N, 0.05, !addNoise)
		build, cdurs, ndurs, heap := benchScale(ctx, vecs, *nq, before)
		fmt.Printf("%d\t%s\t%s\t%s\t%s\t%.0f MB\n", N, build, avg(cdurs), maxDur(cdurs), avg(ndurs), heap)
	}
}

// seedVectors 走一遍 ingest 的切分流程，取真实语料的嵌入向量做种子（全命中磁盘缓存）。
func seedVectors(ctx context.Context, emb *embedding.Client) ([][]float64, int) {
	files, err := filepath.Glob("docs/*.md")
	if err != nil || len(files) == 0 {
		log.Fatalf("找不到语料: %v", err)
	}
	var texts []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("读取 %s: %v", f, err)
		}
		for _, c := range chunk.ByHeading(string(data), 300) {
			texts = append(texts, c.Text)
		}
	}
	vecs, st, err := emb.EmbedWithStats(ctx, texts)
	if err != nil {
		log.Fatalf("嵌入语料失败: %v", err)
	}
	fmt.Printf("种子语料：%d 篇 %d 块（缓存命中 %d，回源 %d）\n", len(files), len(texts), st.Hit, st.Miss)
	return vecs, len(vecs[0])
}

// synth 以种子向量为基底，循环取种子加小扰动后归一化，合成 N 块；
// pure=true 时不加扰动（真实档原样拷贝）。
func synth(seeds [][]float64, N int, eps float64, pure bool) [][]float32 {
	rnd := rand.New(rand.NewSource(42)) // 固定种子，结果可复现
	out := make([][]float32, N)
	for i := range out {
		s := seeds[i%len(seeds)]
		v := make([]float32, len(s))
		var norm float64
		for j := range v {
			x := float32(s[j])
			if !pure {
				x += float32(eps * rnd.NormFloat64())
			}
			v[j] = x
			norm += float64(x) * float64(x)
		}
		inv := float32(1 / math.Sqrt(norm))
		for j := range v {
			v[j] *= inv
		}
		out[i] = v
	}
	return out
}

// benchScale 在给定向量集上测：chromem-go 建库耗时、查询延迟、堆内存增量，
// 并用单线程暴力余弦做对照（同样查询集）。
// Windows 的时钟粒度对亚毫秒测量不可靠：每次查询重复 reps 遍取平均，
// reps 按规模反比缩放，保证单次测量总时长在十毫秒级以上。
func benchScale(ctx context.Context, vecs [][]float32, nq int, before runtime.MemStats) (build time.Duration, cdurs, ndurs []time.Duration, heapMB float64) {
	db := chromem.NewDB()
	col, err := db.CreateCollection("bench", nil, func(context.Context, string) ([]float32, error) {
		return nil, fmt.Errorf("bench: 不应走到嵌入函数") // 全部文档自带向量
	})
	if err != nil {
		log.Fatalf("建集合: %v", err)
	}
	docs := make([]chromem.Document, len(vecs))
	for i, v := range vecs {
		docs[i] = chromem.Document{ID: fmt.Sprintf("synth#%d", i), Content: "合成块", Embedding: v}
	}
	t := time.Now()
	if err := col.AddDocuments(ctx, docs, runtime.NumCPU()); err != nil {
		log.Fatalf("建库: %v", err)
	}
	build = time.Since(t)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heapMB = float64(after.HeapAlloc-before.HeapAlloc) / (1 << 20)

	reps := 200_000/len(vecs) + 1 // 375 块 → 534 遍，5 万块 → 5 遍
	queries := perturb(vecs[0], nq, 0.05) // 查询向量：种子流形附近扰动生成
	for _, q := range queries {
		t := time.Now()
		for r := 0; r < reps; r++ {
			if _, err := col.QueryEmbedding(ctx, q, 3, nil, nil); err != nil {
				log.Fatalf("查询: %v", err)
			}
		}
		cdurs = append(cdurs, time.Since(t)/time.Duration(reps))

		t = time.Now()
		for r := 0; r < reps; r++ {
			bruteTop3(vecs, q)
		}
		ndurs = append(ndurs, time.Since(t)/time.Duration(reps))
	}
	return build, cdurs, ndurs, heapMB
}

// perturb 以一条 float32 向量为基底，加小扰动后归一化，生成 n 条查询向量。
func perturb(seed []float32, n int, eps float64) [][]float32 {
	rnd := rand.New(rand.NewSource(7)) // 固定种子，查询集可复现
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, len(seed))
		var norm float64
		for j := range v {
			x := seed[j] + float32(eps*rnd.NormFloat64())
			v[j] = x
			norm += float64(x) * float64(x)
		}
		inv := float32(1 / math.Sqrt(norm))
		for j := range v {
			v[j] *= inv
		}
		out[i] = v
	}
	return out
}

// bruteTop3 是第 1 篇 demo 的单线程暴力余弦（向量均已归一化，点积即相似度）。
func bruteTop3(vecs [][]float32, q []float32) [3]float32 {
	var top [3]float32
	for _, v := range vecs {
		var dot float32
		for j := range v {
			dot += v[j] * q[j]
		}
		if dot > top[0] { // 维护有序前三名
			top[0], top[1], top[2] = dot, top[0], top[1]
		} else if dot > top[1] {
			top[1], top[2] = dot, top[1]
		} else if dot > top[2] {
			top[2] = dot
		}
	}
	return top
}

// stat 输出 min/avg/max 汇总。
func stat(ds []time.Duration) string {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return fmt.Sprintf("min %s / avg %s / max %s", ds[0], sum/time.Duration(len(ds)), ds[len(ds)-1])
}

func avg(ds []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

func maxDur(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds[1:] {
		if d > m {
			m = d
		}
	}
	return m
}

// dirSize 递归统计目录体积，返回人类可读形式。
func dirSize(root string) string {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	switch {
	case total >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(total)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(total)/(1<<10))
	}
}
