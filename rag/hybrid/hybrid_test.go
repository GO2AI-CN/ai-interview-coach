package hybrid

import (
	"testing"

	"ai-interview-coach/rag/chunk"
)

func TestTokenizeCJKBigram(t *testing.T) {
	got := Tokenize("向量检索")
	want := []string{"向量", "量检", "检索"}
	if len(got) != len(want) {
		t.Fatalf("Tokenize(向量检索) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tokenize(向量检索)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTokenizeMixed(t *testing.T) {
	got := Tokenize("怎么通过 HTTP 提供服务")
	want := []string{"怎么", "么通", "通过", "http", "提供", "供服", "服务"}
	if len(got) != len(want) {
		t.Fatalf("Tokenize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tokenize[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTokenizeSingleCJKAndCase(t *testing.T) {
	got := Tokenize("好 Go") // 单字兜底 unigram；英文小写化
	if len(got) != 2 || got[0] != "好" || got[1] != "go" {
		t.Fatalf("Tokenize(好 Go) = %v, want [好 go]", got)
	}
}

func TestBM25KeywordBeatsLength(t *testing.T) {
	// 短文档精确含查询词，长文档只是话题沾边——BM25 必须把短文档排前面
	chunks := []chunk.Chunk{
		{Text: "MCP 服务的远程传输基于 HTTP 与 SSE，带鉴权", File: "a.md", Seq: 0},
		{Text: "MCP 是模型上下文协议，解决工具接入问题。协议定义了客户端与服务端的交互，" +
			"包括初始化、工具调用、资源读取等等，内容很长但和具体传输方式无关。", File: "b.md", Seq: 0},
	}
	ix := NewBM25(chunks)
	got := ix.Search("HTTP 传输")
	if len(got) == 0 || got[0].Chunk.File != "a.md" {
		t.Fatalf("BM25 top1 = %+v, want a.md", got)
	}
}

func TestBM25EmptyAndNoMatch(t *testing.T) {
	ix := NewBM25([]chunk.Chunk{{Text: "你好世界", File: "a.md"}})
	if got := ix.Search("完全没有的词zzz"); len(got) != 0 {
		t.Fatalf("无命中应返回空，got %v", got)
	}
	if got := NewBM25(nil).Search("任意"); len(got) != 0 {
		t.Fatalf("空语料应返回空，got %v", got)
	}
}

func TestRRFFusion(t *testing.T) {
	a := chunk.Chunk{Text: "甲", File: "a.md", Seq: 0}
	b := chunk.Chunk{Text: "乙", File: "b.md", Seq: 0}
	c := chunk.Chunk{Text: "丙", File: "c.md", Seq: 0}
	// 向量路：甲>乙；BM25 路：丙>甲。两路都高名的甲应当融合后第一。
	vec := []Scored{{Chunk: a, Score: 0.9}, {Chunk: b, Score: 0.8}}
	kw := []Scored{{Chunk: c, Score: 12}, {Chunk: a, Score: 3}}
	got := RRF(60, vec, kw)
	if len(got) != 3 {
		t.Fatalf("融合后应有 3 条，got %d", len(got))
	}
	if got[0].Chunk.File != "a.md" {
		t.Fatalf("RRF top1 = %s, want a.md（两路都靠前者应胜出）", got[0].Chunk.File)
	}
	// 乙只在向量路第 2 名，丙只在关键词路第 1 名：k=60 时
	// 丙得分 1/61 > 乙得分 1/62，丙应排乙前
	if got[1].Chunk.File != "c.md" || got[2].Chunk.File != "b.md" {
		t.Fatalf("RRF 2/3 名 = %s,%s，want c.md,b.md", got[1].Chunk.File, got[2].Chunk.File)
	}
}
