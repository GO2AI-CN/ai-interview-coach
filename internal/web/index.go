package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// handleIndex 返回内置的单页面试界面。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}
