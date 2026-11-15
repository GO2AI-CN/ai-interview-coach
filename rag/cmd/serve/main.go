// serve 是系列第 7 篇《生产化》的 HTTP 问答服务，也是 Docker 部署的载体：
// 把第 5 篇的问答链路（检索 → 拼 prompt → 生成 → 溯源）包成两个端点——
//
//	GET  /healthz                → 200 "ok"（容器健康检查用）
//	POST /ask {"question":"..."} → {"answer":"...","sources":["[1] 《..》§..", ...]}
//
// 服务无状态、单轮问答，多实例前面挂负载均衡即可水平扩展；
// SSE 流式版本见第 5 篇的 chat CLI，这里求部署最简单，用一次性 JSON 响应。
//
// 运行（仓库根目录，需先 go run ./rag/cmd/sync 建库）：
//
//	go run ./rag/cmd/serve -addr :8080
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"ai-interview-coach/rag/embedding"
	"ai-interview-coach/rag/prompt"
	"ai-interview-coach/rag/store"
)

type askRequest struct {
	Question string `json:"question"`
}

type askResponse struct {
	Answer  string   `json:"answer"`
	Sources []string `json:"sources"`
}

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	dbPath := flag.String("db", ".vecdb", "向量库目录")
	cache := flag.String("cache", ".embed-cache", "embedding 磁盘缓存目录")
	topK := flag.Int("k", 3, "每问检索命中块数")
	flag.Parse()
	_ = godotenv.Load()

	st, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("打开向量库: %v", err)
	}
	if n := st.Count(context.Background()); n <= 0 {
		log.Fatal("向量库为空：先跑 go run ./rag/cmd/sync")
	}
	emb := embedding.New(os.Getenv("EMBED_API_KEY"), os.Getenv("EMBED_BASE_URL"),
		os.Getenv("EMBED_MODEL"), *cache)
	model := os.Getenv("COACH_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	cli := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithBaseURL(os.Getenv("OPENAI_BASE_URL")),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var req askRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Question == "" {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		ctx := r.Context()

		qvecs, err := emb.Embed(ctx, []string{req.Question})
		if err != nil {
			http.Error(w, `{"error":"embed failed"}`, http.StatusBadGateway)
			return
		}
		hits, err := st.Search(ctx, qvecs[0], *topK, nil)
		if err != nil {
			http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
			return
		}
		system, user := prompt.Build(req.Question, hits)
		resp, err := cli.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: openai.ChatModel(model),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(system),
				openai.UserMessage(user),
			},
		})
		if err != nil {
			http.Error(w, `{"error":"llm failed"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(askResponse{
			Answer:  resp.Choices[0].Message.Content,
			Sources: prompt.Sources(hits),
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("serve: 库内 %d 块，监听 %s", st.Count(context.Background()), *addr)
	log.Fatal(srv.ListenAndServe())
}
