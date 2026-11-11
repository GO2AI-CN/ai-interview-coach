// Package chunk 提供三种文档切分策略，用于系列第三篇第 3 讲《文档切分实战》的对比实测：
//
//	BySize      固定字符窗口：每 size 字符一块，块间重叠 overlap 字符（可能切断句子）
//	ByParagraph 按空行分段再贪心合并：块内是完整段落，跨块携带一个段落作重叠
//	ByHeading   按 Markdown 标题切分：每个 ## 小节一块，元数据带标题路径；超长小节内部再按窗口切
//
// 三个函数都返回 []Chunk，带 File / Head / Seq 元数据——检索命中后要能告诉读者"出自哪篇哪节"。
package chunk

import (
	"path/filepath"
	"strings"
)

// Chunk 是切分后的最小检索单元。
type Chunk struct {
	Text string `json:"text"` // 正文内容
	File string `json:"file"` // 来源文件名，如 2026-11-06-go-rag-1-intro.md
	Head string `json:"head"` // 所属 Markdown 标题路径，如 "四、追问策略"
	Seq  int    `json:"seq"`  // 在文件内的序号，用于排序和去重
}

// BySize 固定窗口切分：实现最简，但块可能从句子中间断开，语义不完整。
// 按 rune（字符）切而不是按 byte，避免把中文切成半个字产生乱码。
func BySize(text string, size, overlap int) []Chunk {
	text = strings.Join(strings.Fields(text), " ") // 压缩空白，避免块里全是换行
	runes := []rune(text)
	if size <= 0 || len(runes) <= size {
		return []Chunk{{Text: text}}
	}
	stride := size - overlap
	if stride <= 0 {
		stride = size // overlap 不得大于等于 size，否则死循环
	}
	var out []Chunk
	for start := 0; start < len(runes); start += stride {
		end := min(start+size, len(runes))
		out = append(out, Chunk{Text: string(runes[start:end])})
		if end == len(runes) {
			break
		}
	}
	return out
}

// ByParagraph 按空行分段，再贪心合并到 size 以内；块满开新块时，
// 先把上一块的最后一段复制到新块开头——跨块边界的上下文不被切断。
func ByParagraph(text string, size int) []Chunk {
	var paras []string
	for _, p := range strings.Split(text, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			paras = append(paras, p)
		}
	}
	var out []Chunk
	var cur []string // 当前块收集到的段落
	sewn := 0        // 当前块里"重叠进来的旧段落"数量，避免无限重复
	for _, p := range paras {
		total := 0
		for _, q := range cur {
			total += len([]rune(q)) + 2
		}
		if len(cur) > sewn && total+len([]rune(p)) > size {
			out = append(out, Chunk{Text: strings.Join(cur, "\n\n")})
			cur = cur[len(cur)-1:] // 旧块最后一段作为新块开头（重叠）
			sewn = 1
		}
		cur = append(cur, p)
	}
	if len(cur) > 0 {
		out = append(out, Chunk{Text: strings.Join(cur, "\n\n")})
	}
	return out
}

// ByHeading 按 Markdown 标题切分：每个 ## 小节自成一块（标题路径记入 Head），
// 小节正文不含标题行本身；小节超过 size 时内部退回固定窗口切分。
func ByHeading(text string, size int) []Chunk {
	lines := strings.Split(text, "\n")
	var out []Chunk
	head, seq := "", 0
	var body strings.Builder
	flush := func() {
		b := strings.TrimSpace(body.String())
		body.Reset()
		if b == "" {
			return
		}
		for _, c := range BySize(b, size, size/10) { // 超长小节内部再切
			out = append(out, Chunk{Text: c.Text, Head: head, Seq: seq})
			seq++
		}
	}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") { // 遇到新标题：先收尾旧小节
			flush()
			head = strings.TrimSpace(strings.TrimPrefix(ln, "## "))
			continue
		}
		if strings.HasPrefix(ln, "# ") { // 文章大标题只进 head，不进正文
			head = strings.TrimSpace(strings.TrimPrefix(ln, "# "))
			continue
		}
		body.WriteString(ln)
		body.WriteString("\n")
	}
	flush()
	return out
}

// TagFile 给一组 chunk 补上来源文件名（各策略函数不感知文件，由调用方统一打标）。
func TagFile(file string, cs []Chunk) []Chunk {
	base := filepath.Base(file)
	for i := range cs {
		cs[i].File = base
	}
	return cs
}
