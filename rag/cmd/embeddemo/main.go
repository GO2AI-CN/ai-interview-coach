// embeddemo 是系列第三篇第 1 讲的教学 demo：
// 不依赖任何外部库，手工把文本变成向量，再用余弦相似度做语义搜索。
// 它只是揭示「向量检索」 mechanics 的玩具——真正的语义向量由神经网络训练得到，
// 第 2 篇会接入真实的 Embedding API。
package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// 语料：本系列第 3 篇起会换成 docs/ 目录下 16 篇真实文章。
var docs = []string{
	"使用 systemd 守护进程让面试教练 7×24 小时运行",
	"把面试历史记录保存到 SQLite 数据库，支持随时回看",
	"MCP 协议让 Agent 以标准方式调用外部工具",
	"通过 SSE 实现网页版面试的流式输出",
}

// naiveEmbed 把文本变成定长向量：统计字符一元组 + 二元组的出现次数。
// 归一化后，两个向量的点积就等于余弦相似度（cosine 函数里直接用）。
func naiveEmbed(text string, dims int) []float64 {
	vec := make([]float64, dims)
	runes := []rune(strings.ToLower(text))
	for i := 0; i < len(runes); i++ {
		vec[int(runes[i])%dims]++ // 一元组：单个字符的贡献
		if i+1 < len(runes) {
			bigram := int(runes[i])<<16 | int(runes[i+1])
			vec[bigram%dims]++ // 二元组：相邻两个字符一起贡献
		}
	}
	return l2norm(vec)
}

// l2norm 把向量长度缩放到 1，之后点积直接等于余弦相似度。
func l2norm(v []float64) []float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	n := math.Sqrt(sum)
	for i := range v {
		v[i] /= n
	}
	return v
}

// dot 计算点积。输入均为单位向量时，返回值即余弦相似度：1 最像，0 无关。
func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func main() {
	queries := []string{"面试历史怎么保存", "怎么让程序一直跑着"}
	for _, q := range queries {
		fmt.Printf("查询：%s\n", q)
		qv := naiveEmbed(q, 4096)

		type hit struct {
			score float64
			doc   string
		}
		var hits []hit
		for _, d := range docs {
			hits = append(hits, hit{dot(qv, naiveEmbed(d, 4096)), d})
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

		for i, h := range hits {
			fmt.Printf("  #%d  %.3f  %s\n", i+1, h.score, h.doc)
		}
	}
}
