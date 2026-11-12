// Package hybrid 是 RAG 系列第 6 篇《检索质量进阶》的配套代码：
// 在纯向量检索之外补上关键词检索（BM25），再用 RRF 把两路排名融合成一路。
//
// 设计要点：
//   - BM25 纯 Go 手写，零依赖：中文按字二元组（bigram）切词，英文数字按单词，
//     不引入分词器——几百块的小语料上足够好，且 go.mod 一行不动；
//   - RRF（Reciprocal Rank Fusion）只看名次不看分数：1/(k+rank)。
//     向量余弦和 BM25 分数量纲完全不同，直接加权必翻车，按名次融合最稳；
//   - 输入输出都是 chunk.Chunk，与 store.Search 的结果天然对齐，
//     融合时以 "文件名#序号" 作为文档身份。
package hybrid

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"ai-interview-coach/rag/chunk"
)

// BM25 的两个经验参数：k1 控制词频饱和度（词出现 3 次后边际收益锐减），
// b 控制文档长度归一化强度（0=不惩罚长文档，1=全额惩罚）。1.5/0.75 是通行默认值。
const (
	k1 = 1.5
	b  = 0.75
)

// Scored 是一条带分数的检索命中，三种检索器（向量/BM25/RRF）共用。
type Scored struct {
	Chunk chunk.Chunk
	Score float64
}

// id 返回文档身份键，与向量库 ingest 时的幂等键约定一致（"文件名#序号"）。
func id(c chunk.Chunk) string {
	return c.File + "#" + strconv.Itoa(c.Seq)
}

// Tokenize 把文本切成检索词：连续的英文/数字run小写化为一个词；
// 连续的中文（含日文韩文）按相邻两字切 bigram，单字兜底成 unigram。
// "HTTP 服务" → ["http", "服务"]；"怎么通过HTTP" → ["怎么", "么通", "通过", "http"]。
func Tokenize(text string) []string {
	var tokens []string
	var latin strings.Builder
	var cjk []rune
	flushLatin := func() {
		if latin.Len() > 0 {
			tokens = append(tokens, latin.String())
			latin.Reset()
		}
	}
	flushCJK := func() {
		if len(cjk) == 1 { // 单字不成对，直接作为词
			tokens = append(tokens, string(cjk))
		}
		for i := 0; i+1 < len(cjk); i++ {
			tokens = append(tokens, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r): // 中日韩表意文字走 bigram
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r): // 拉丁字母/数字聚成单词
			flushCJK()
			latin.WriteRune(unicode.ToLower(r))
		default: // 标点空白是天然分界
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}

// BM25 是一个建在内存里的关键词倒排索引，语料就是切片时得到的全量 chunk。
type BM25 struct {
	docs  []chunk.Chunk
	tfs   []map[string]int // 每块文档的词频表
	dls   []int            // 每块文档的词数
	df    map[string]int   // 每个词的文档频率
	avgdl float64          // 平均文档长度
}

// NewBM25 对 chunks 建立倒排索引。语料几百块时构建是微秒级，无需持久化。
func NewBM25(chunks []chunk.Chunk) *BM25 {
	ix := &BM25{docs: chunks, df: map[string]int{}}
	var total int
	for _, c := range chunks {
		tf := map[string]int{}
		for _, t := range Tokenize(c.Text) {
			tf[t]++
		}
		ix.tfs = append(ix.tfs, tf)
		dl := 0
		for _, n := range tf {
			dl += n
		}
		ix.dls = append(ix.dls, dl)
		total += dl
		for t := range tf {
			ix.df[t]++ // 每词每文档只计一次
		}
	}
	if len(chunks) > 0 {
		ix.avgdl = float64(total) / float64(len(chunks))
	}
	return ix
}

// Search 返回全部得分 > 0 的文档，按 BM25 分数降序——一路完整排名，供 RRF 融合。
// 经典 BM25 公式：Σ IDF(t) · tf·(k1+1) / (tf + k1·(1-b+b·dl/avgdl))
func (x *BM25) Search(query string) []Scored {
	n := float64(len(x.docs))
	var out []Scored
	for i, tf := range x.tfs {
		var score float64
		for _, t := range Tokenize(query) {
			f, ok := tf[t]
			if !ok {
				continue
			}
			// 变体 IDF，词越稀有权重越高，且永远非负
			idf := math.Log(1 + (n-float64(x.df[t])+0.5)/(float64(x.df[t])+0.5))
			tfn := float64(f) * (k1 + 1) / (float64(f) + k1*(1-b+b*float64(x.dls[i])/x.avgdl))
			score += idf * tfn
		}
		if score > 0 {
			out = append(out, Scored{Chunk: x.docs[i], Score: score})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// RRF 把多路排名按 1/(k+rank) 融合：每路内部名次从 1 开始，同一文档在多路
// 出现时分数累加。k=60 是论文原值，名次靠前时区分度大、靠后时趋平。
// 返回按融合分降序的完整结果；rankings 之间允许长度不同、元素不同。
func RRF(k float64, rankings ...[]Scored) []Scored {
	type acc struct {
		c     chunk.Chunk
		score float64
	}
	fused := map[string]*acc{}
	for _, ranking := range rankings {
		for i, s := range ranking {
			key := id(s.Chunk)
			a, ok := fused[key]
			if !ok {
				a = &acc{c: s.Chunk}
				fused[key] = a
			}
			a.score += 1 / (k + float64(i) + 1) // i 从 0 开始，名次 = i+1
		}
	}
	out := make([]Scored, 0, len(fused))
	for _, a := range fused {
		out = append(out, Scored{Chunk: a.c, Score: a.score})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
