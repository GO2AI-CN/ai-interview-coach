// rerankdemo 是系列第 6 篇《检索质量进阶》的重排序演示 CLI：
// 先跑 RRF 混合检索取 top-10 候选，再让大模型逐块打 0~10 相关分并重排，
// 打印重排前后名次——看混合检索救回来的正主块，能否再被推到第一。
//
// 评测发现：RRF 能把正确的「文件」救进 top-3，但文件内最准的「小节」
// 未必排第一（引用溯源以块为单位，块不准出处就不准）。rerank 补这最后一棒。
//
// 运行（仓库根目录，需先 go run ./rag/cmd/ingest 建库）：
//
//	go run ./rag/cmd/rerankdemo "MCP server 怎么通过 HTTP 对外提供服务"
//	go run ./rag/cmd/rerankdemo -n 10 "你的问题"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"ai-interview-coach/rag/chunk"
	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/hybrid"
	"ai-interview-coach/rag/store"
)

func main() {
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	n := flag.Int("n", 10, "候选块数（rerank 的输入规模）")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("用法：go run ./rag/cmd/rerankdemo [-n N] \"你的问题\"")
	}
	question := flag.Arg(0)
	_ = godotenv.Load()

	ctx := context.Background()
	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	total := st.Count(ctx)
	if total <= 0 {
		log.Fatal("向量库为空：先跑 go run ./rag/cmd/ingest")
	}

	// 1. 混合检索：向量全量排名 + BM25 排名 → RRF 融合 → 取 top-n 候选
	emb := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), ".embed-cache")
	qvecs, err := emb.Embed(ctx, []string{question})
	if err != nil {
		log.Fatalf("嵌入问题失败: %v", err)
	}
	full, err := st.Search(ctx, qvecs[0], total, nil)
	if err != nil {
		log.Fatalf("检索失败: %v", err)
	}
	vec := make([]hybrid.Scored, len(full))
	cs := make([]chunk.Chunk, len(full))
	for i, h := range full {
		vec[i] = hybrid.Scored{Chunk: h.Chunk, Score: float64(h.Score)}
		cs[i] = h.Chunk
	}
	bm := hybrid.NewBM25(cs)
	cands := hybrid.RRF(60, vec, bm.Search(question))
	if len(cands) > *n {
		cands = cands[:*n]
	}
	fmt.Printf("rerankdemo: 库内 %d 块，候选 %d 块，问题：%s\n\n", total, len(cands), question)
	fmt.Println("—— 重排前（RRF 名次）——")
	for i, c := range cands {
		fmt.Printf("%2d. %s § %s\n", i+1, c.Chunk.File, c.Chunk.Head)
	}

	// 2. LLM 打分：一次请求让所有候选同尺度可比（0~10），返回 JSON 数组
	model := os.Getenv("COACH_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	cli := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithBaseURL(os.Getenv("OPENAI_BASE_URL")),
	)
	scores, err := scoreAll(ctx, cli, model, question, cands)
	if err != nil {
		log.Fatalf("LLM 打分失败: %v", err)
	}

	// 3. 按打分重排，打印名次变化
	type row struct {
		c     hybrid.Scored
		score float64
		from  int // 重排前名次（1 起）
	}
	rows := make([]row, len(cands))
	for i := range cands {
		rows[i] = row{c: cands[i], score: scores[i], from: i + 1}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	fmt.Println("\n—— 重排后（LLM 打分）——")
	for i, r := range rows {
		fmt.Printf("%2d. [%4.1f 分] %s § %s（原 #%d）\n", i+1, r.score, r.c.Chunk.File, r.c.Chunk.Head, r.from)
	}
}

// scoreAll 让模型给每个候选块打 0~10 分，一次请求完成（分数同尺度可比）。
// 输出约定为 JSON 数组，长度等于候选数；模型加 markdown 围栏也能解析。
func scoreAll(ctx context.Context, cli openai.Client, model, question string, cands []hybrid.Scored) ([]float64, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "问题：%s\n\n下面 %d 个资料块，请评估每块对回答该问题的相关程度，"+
		"打 0~10 的整数分（10=直接给出答案，0=完全无关）。 "+
		"只输出一个 JSON 数组，长度 %d，例如 [3,7,0]，不要输出任何其他内容。\n\n", question, len(cands), len(cands))
	for i, c := range cands {
		text := []rune(c.Chunk.Text)
		if len(text) > 400 { // 块最长 400 字，控制单次请求体量
			text = text[:400]
		}
		fmt.Fprintf(&b, "[%d] %s\n\n", i+1, string(text))
	}
	resp, err := cli.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel(model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(b.String()),
		},
	})
	if err != nil {
		return nil, err
	}
	out := resp.Choices[0].Message.Content
	// 模型偶尔包 ```json 围栏或在前后啰嗦，截取第一个 [...] 区间解析
	start := strings.Index(out, "[")
	end := strings.LastIndex(out, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("模型输出不含 JSON 数组: %q", out)
	}
	var scores []float64
	if err := json.Unmarshal([]byte(out[start:end+1]), &scores); err != nil {
		return nil, fmt.Errorf("解析分数失败: %w（原始输出 %q）", err, out)
	}
	if len(scores) != len(cands) {
		return nil, fmt.Errorf("分数数 %d 与候选数 %d 不一致", len(scores), len(cands))
	}
	return scores, nil
}
