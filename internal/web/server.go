// Package web 网页版面试界面：内置 HTTP 服务 + SSE 流式输出。
// 浏览器打开即用，面试点评逐字显示，体验接近真实面试官开口说话。
package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"ai-interview-coach/internal/coach"
	"ai-interview-coach/internal/llm"
	"ai-interview-coach/internal/report"
)

// Server 持有 LLM 客户端与全部面试会话（内存版，单机单用户使用）。
type Server struct {
	client llm.Client

	mu       sync.Mutex
	sessions map[string]*coach.Interview
}

func NewServer(client llm.Client) *Server {
	return &Server{client: client, sessions: map[string]*coach.Interview{}}
}

// ListenAndServe 启动网页版，addr 形如 ":8080"。
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)           // 面试页面
	mux.HandleFunc("POST /api/start", s.handleStart) // 开始面试（生成计划）
	mux.HandleFunc("GET /api/answer", s.handleAnswer) // SSE：提交回答，流式返回点评
	mux.HandleFunc("GET /api/question", s.handleQuestion) // 取当前题
	mux.HandleFunc("GET /api/report", s.handleReport)     // 取评分报告
	fmt.Printf("网页版面试教练已启动：http://localhost%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

// handleStart 生成面试计划，返回首题与会话 ID。
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Position string `json:"position"`
		Resume   string `json:"resume"` // 简历 PDF 路径，可选
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Position == "" {
		writeJSON(w, 400, map[string]string{"error": "position 必填"})
		return
	}

	it, err := coach.NewInterview(r.Context(), s.client, req.Position, "")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	id := newSessionID()
	s.mu.Lock()
	s.sessions[id] = it
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"session": id, "question": it.CurrentQuestion(), "index": 1, "total": len(it.State.Plan.Questions)})
}

// handleAnswer 核心接口：SSE 流式返回面试官点评。
// 点评是面试体验的核心——逐字输出比一次性弹一大段文字更像真人。
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	it := s.lookup(r)
	if it == nil {
		writeJSON(w, 404, map[string]string{"error": "会话不存在"})
		return
	}
	answer := r.URL.Query().Get("a")
	if answer == "" {
		writeJSON(w, 400, map[string]string{"error": "回答为空"})
		return
	}

	// 1) 结构化评估（JSON 模式，非流式：格式必须严格）
	followUp, followPrompt, err := it.Answer(r.Context(), answer)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	ev := it.State.Pending.Evaluation
	_ = coach.Save(it.State, coach.DefaultStateFile)

	// 2) 点评文案流式输出（SSE）
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "不支持流式响应"})
		return
	}
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	// 让模型把评估结果"说人话"，逐 token 推给浏览器
	commentary := fmt.Sprintf("考察要点命中 %.0f%%，置信度 %d/5。%s",
		ev.Coverage*100, ev.Confidence, ev.Suggestion)
	sys := "你是面试官，用 1~3 句话口语化点评候选人刚才的回答，语气直接、具体。"
	var streamErr error
	full, streamErr := s.client.ChatStream(r.Context(), sys, commentary+"\n\n候选人回答："+answer,
		func(tok string) { send("token", map[string]string{"t": tok}) })

	// 3) 汇总事件：追问 or 下一题 or 结束
	result := map[string]any{"commentary": full, "followup": followUp, "coverage": ev.Coverage, "confidence": ev.Confidence}
	if followUp {
		result["question"] = followPrompt
		send("result", result)
	} else if q := it.CurrentQuestion(); q != nil {
		result["question"] = q
		result["index"] = it.State.Cursor + 1
		send("result", result)
	} else {
		send("result", result) // 题目已尽，前端接着拉 /api/report
	}
	if streamErr != nil {
		send("error", map[string]string{"error": streamErr.Error()})
	}
	send("done", map[string]bool{})
}

func (s *Server) handleQuestion(w http.ResponseWriter, r *http.Request) {
	it := s.lookup(r)
	if it == nil {
		writeJSON(w, 404, map[string]string{"error": "会话不存在"})
		return
	}
	q := it.CurrentQuestion()
	if q == nil {
		writeJSON(w, 200, map[string]any{"done": true})
		return
	}
	writeJSON(w, 200, map[string]any{"question": q, "index": it.State.Cursor + 1, "total": len(it.State.Plan.Questions)})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	it := s.lookup(r)
	if it == nil {
		writeJSON(w, 404, map[string]string{"error": "会话不存在"})
		return
	}
	rep, err := it.Finish(r.Context())
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"report": report.Markdown(rep), "overall": rep.Overall})
}

func (s *Server) lookup(r *http.Request) *coach.Interview {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[r.URL.Query().Get("session")]
}

func newSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// 供测试注入会话
func (s *Server) SetSessionForTest(id string, it *coach.Interview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = it
}
