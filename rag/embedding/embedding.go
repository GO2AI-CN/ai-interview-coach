// Package embedding 是对 OpenAI 兼容 /v1/embeddings 端点的薄封装：
// 批量调用、磁盘缓存（同一文本绝不重复扣费）、余弦相似度计算。
// 系列第 2 篇《Embedding API 实战》的配套代码。
package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Client 绑定一个兼容端点 + 一个模型 + 一个本地缓存目录。
type Client struct {
	cli      openai.Client
	model    string
	cacheDir string
}

// New 创建客户端；baseURL 为空时走 OpenAI 官方，否则指向兼容端点（硅基流动/DeepSeek 等）。
// cacheDir 非空时会创建该目录，用于落盘缓存嵌入结果。
func New(apiKey, baseURL, model, cacheDir string) *Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if cacheDir != "" {
		_ = os.MkdirAll(cacheDir, 0o755) // 缓存目录不存在则创建
	}
	return &Client{cli: openai.NewClient(opts...), model: model, cacheDir: cacheDir}
}

// Embed 把一组文本变成向量。命中磁盘缓存的文本不再请求 API；
// 未命中的按 32 条一批合并请求，减少往返次数。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	vecs := make([][]float64, len(texts))
	var missIdx []int // 未命中缓存的下标
	for i, t := range texts {
		if v, ok := c.load(t); ok {
			vecs[i] = v
		} else {
			missIdx = append(missIdx, i)
		}
	}
	const batch = 32 // 单请求条数上限，避免超大请求超时
	for start := 0; start < len(missIdx); start += batch {
		end := min(start+batch, len(missIdx))
		inputs := make([]string, 0, end-start)
		for _, i := range missIdx[start:end] {
			inputs = append(inputs, texts[i])
		}
		resp, err := c.cli.Embeddings.New(ctx, openai.EmbeddingNewParams{
			Model: openai.EmbeddingModel(c.model),
			Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
		})
		if err != nil {
			return nil, err
		}
		for j, item := range resp.Data {
			idx := missIdx[start+j]
			vecs[idx] = item.Embedding
			c.store(texts[idx], item.Embedding) // 落盘，下次免费
		}
	}
	return vecs, nil
}

// cachePath 用 模型名+文本 的哈希做文件名，同模型同文本永远映射到同一缓存文件。
func (c *Client) cachePath(text string) string {
	sum := sha256.Sum256([]byte(c.model + "\x00" + text))
	return filepath.Join(c.cacheDir, hex.EncodeToString(sum[:])+".json")
}

func (c *Client) load(text string) ([]float64, bool) {
	b, err := os.ReadFile(c.cachePath(text))
	if err != nil {
		return nil, false
	}
	var v []float64
	if json.Unmarshal(b, &v) != nil {
		return nil, false
	}
	return v, true
}

func (c *Client) store(text string, v []float64) {
	if c.cacheDir == "" {
		return
	}
	b, _ := json.Marshal(v)
	_ = os.WriteFile(c.cachePath(text), b, 0o644) // 缓存写失败不阻塞主流程
}

// Cosine 计算两个向量的余弦相似度，值域 [-1,1]，越接近 1 语义越相近。
// 不做归一化假设，对任何 embedding 输出都适用（与 rag/cmd/embeddemo 的 dot 不同）。
func Cosine(a, b []float64) (float64, error) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, errors.New("向量维度不一致或为空")
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb)), nil
}
