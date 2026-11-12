// Package store 是 RAG 系列的向量库存储层（第 4 篇配套代码），
// 底层用 chromem-go v0.7.0——嵌入式向量库，进程内运行，无需单独部署服务。
//
// 设计要点：
//   - 嵌入复用第 2 篇的 embedding 包（bge-m3 + 磁盘缓存），向量算好后
//     连文档一起交给 chromem-go；它见到 Embedding 字段非空就跳过自己的
//     嵌入函数，只做归一化后入库——谁嵌入、谁存储，职责分离；
//   - 检索走 QueryEmbedding 而不是 Query：查询向量我们自己算（同样吃缓存），
//     v0.7 起 SDK 会替未归一化的查询向量自动归一化，不再要求调用方预处理；
//   - 文档 ID 约定 "文件名#序号"，metadata 带 file/head/seq，
//     第 5 篇拼 prompt 时靠它们做引用溯源，where 过滤也走 metadata。
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/philippgille/chromem-go"

	"ai-interview-coach/rag/chunk"
)

// errNoEmbedFunc 是集合的占位嵌入函数：本包所有写入都带预计算向量，
// 真走到这里说明调用方忘了先嵌入——直接报错比静默调用默认 OpenAI 函数安全。
var errNoEmbedFunc = errors.New("store: 写入前必须先用 embedding 包计算向量")

// Store 包装一个持久化的 chromem-go 数据库和单个集合。
type Store struct {
	db  *chromem.DB
	col *chromem.Collection
}

// Result 是一次检索命中：原始块元数据 + 余弦相似度。
type Result struct {
	Chunk chunk.Chunk
	Score float32
}

// Open 打开（不存在则创建）持久化向量库。path 是目录，compress=true 落盘 gzip。
func Open(path string, compress bool) (*Store, error) {
	db, err := chromem.NewPersistentDB(path, compress)
	if err != nil {
		return nil, err
	}
	// 持久化不保存函数，重新打开时必须再给一次嵌入函数；我们的占位函数只会报错
	col, err := db.GetOrCreateCollection("docs", nil, func(context.Context, string) ([]float32, error) {
		return nil, errNoEmbedFunc
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// AddChunks 把切分块连同预计算向量写入集合。chunks 与 vectors 必须等长。
func (s *Store) AddChunks(ctx context.Context, chunks []chunk.Chunk, vectors [][]float64) error {
	if len(chunks) != len(vectors) {
		return fmt.Errorf("chunks(%d) 与 vectors(%d) 数量不一致", len(chunks), len(vectors))
	}
	docs := make([]chromem.Document, 0, len(chunks))
	for i, c := range chunks {
		// float64 → float32：存储层只吃 32 位，相似度精度足够
		vec := make([]float32, len(vectors[i]))
		for j, v := range vectors[i] {
			vec[j] = float32(v)
		}
		docs = append(docs, chromem.Document{
			ID:        c.File + "#" + strconv.Itoa(c.Seq), // 幂等键：重复 ingest 同一块覆盖更新
			Content:   c.Text,
			Embedding: vec, // 非空 → chromem-go 跳过嵌入函数，仅归一化
			Metadata: map[string]string{
				"file": c.File,
				"head": c.Head,
				"seq":  strconv.Itoa(c.Seq),
			},
		})
	}
	return s.col.AddDocuments(ctx, docs, 8) // 8 路并发嵌入计算——对我们是纯向量拷贝，开销可忽略
}

// Search 用预计算的查询向量做 top-k 检索，可附加 metadata 等值过滤（如指定 file）。
func (s *Store) Search(ctx context.Context, queryVec []float64, topK int, filter map[string]string) ([]Result, error) {
	vec := make([]float32, len(queryVec))
	for i, v := range queryVec {
		vec[i] = float32(v)
	}
	hits, err := s.col.QueryEmbedding(ctx, vec, topK, filter, nil)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(hits))
	for _, h := range hits {
		seq, _ := strconv.Atoi(h.Metadata["seq"])
		results = append(results, Result{
			Score: h.Similarity,
			Chunk: chunk.Chunk{
				Text: h.Content,
				File: h.Metadata["file"],
				Head: h.Metadata["head"],
				Seq:  seq,
			},
		})
	}
	return results, nil
}

// Count 返回集合内文档数，ingest 后自检用。
func (s *Store) Count(context.Context) int {
	return s.col.Count()
}

// Delete 按文档 ID 删除（文档变更时增量索引用，第 7 篇接上）。
func (s *Store) Delete(ctx context.Context, ids ...string) error {
	return s.col.Delete(ctx, nil, nil, ids...)
}
