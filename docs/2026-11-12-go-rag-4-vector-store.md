# 向量库存哪？四类方案对比与 chromem-go 实战

这个系列写到第三篇，我们已经把 RAG 的前两道关都趟过去了：Embedding API 调通、bge-m3 的真实效果测过，16 篇文章三种切法也对比完了。今天收拢最后一根线——**切出来的几百个 chunk，连同它们的向量，到底存哪？**

很多人的第一反应是：上向量数据库，docker pull 一个 Qdrant 再说。先打住。选型选错，后面每一步都在还债。这篇先把四类存储方案摆开对比，然后用一个零依赖的嵌入式向量库 chromem-go，把「索引构建 → 持久化 → top-k 检索」整条链路在 Go 进程内跑通。

## 一、向量库选型：一张表想清楚

向量存储的本质需求就三条：存得下、查得快、重启不丢。围绕这三条，业界方案分四类，按「重」的程度从轻到重排：

| 方案 | 代表 | 部署方式 | 持久化 | 规模量级 | 什么时候选 |
| --- | --- | --- | --- | --- | --- |
| 内存裸结构 | map + 手写余弦（本系列第 1 篇 demo） | 无 | 无，重启全丢 | 千级以下 | 学原理、跑 demo |
| SQLite + 向量扩展 | sqlite-vec | 嵌入式，一个文件 | 有 | 十万级 | 已有 SQLite 体系，要 SQL 过滤 |
| 嵌入式向量库 | chromem-go | 嵌入式，纯 Go 库 | 有（可选压缩） | 万级以下 | Go 单体应用快速落地 |
| 独立向量数据库 | Milvus / Qdrant / pgvector | 独立服务（pgvector 寄生 PG） | 有 | 百万级以上 | 多租户、分布式、海量数据 |

三条选型心法，收藏这一张表就够：

1. **看数据量**。我们的场景——16 篇文章切出 260 个 chunk——离任何「数据库」的门槛都还差着数量级，嵌入式方案绰绰有余；
2. **看运维预算**。独立服务意味着多一个进程要部署、监控、备份。个人项目和中小团队，进程内一个库能搞定的，别给自己加戏；
3. **看过滤需求**。如果检索时要叠复杂的标量条件（时间范围、多字段组合），SQL 系的 sqlite-vec / pgvector 更顺手；只是按来源文件做个等值过滤，嵌入式库完全够。

这个系列用 chromem-go，不是因为它是「最强」，而是它恰好卡在甜点区：零依赖、纯 Go、和 SQLite 一样随进程生死，以后规模真上去了，Chroma 风格的 API 迁移到 Qdrant 也不疼。

## 二、chromem-go：向量检索的「SQLite 时刻」

chromem-go 的一句话定位：**跑在 Go 进程内的嵌入式向量数据库**——注意它不是 Chroma 的客户端，是一个独立实现，只是借了 Chroma 的接口形状。写这篇时我核对过它的 releases 页，最新版是 **v0.7.0**（2024-09 发布），协议许可证是 MPL-2.0，README 明确说项目处于 beta、v1.0.0 之前可能有 breaking change——版本号要钉死，升级前看 CHANGELOG，这是用它的心态前提。

性能方面，README 的基准数据：中端 CPU 上 1,000 文档查询 0.3ms，100,000 文档 40ms。原理很简单——全部加载进内存，暴力算余弦，没有 ANN 索引。这也划清了它的能力边界：

- 优点：零第三方依赖（go.mod 里没有一个外部库）、可选 gzip 压缩持久化、支持 OpenAI/Ollama 等常见 embedding 接入；
- 边界：**没有 HNSW 之类的近似索引**，十万级以上是它的天花板；metadata 过滤只支持等值，不支持区间和组合条件。

260 个 chunk 对它是零头中的零头。诚实面对边界，比假装它能扛一切重要。

## 三、store 层代码：谁嵌入，谁存储，职责分开

开写代码。项目里 `rag/store/store.go` 三十来行，核心设计是一个**职责分离**：嵌入继续用第 2 篇封装的 bge-m3 + 磁盘缓存，chromem-go 只负责存和查。chromem-go 见到文档的 `Embedding` 字段非空就跳过自己的嵌入函数，直接归一化入库——这意味着换模型、换供应商，存储层一行不用动：

```go
// 打开（不存在则创建）持久化向量库，compress=true 落盘 gzip
func Open(path string, compress bool) (*Store, error) {
	db, err := chromem.NewPersistentDB(path, compress)
	if err != nil {
		return nil, err
	}
	col, err := db.GetOrCreateCollection("docs", nil, func(context.Context, string) ([]float32, error) {
		return nil, errors.New("store: 写入前必须先用 embedding 包计算向量")
	})
	return &Store{db: db, col: col}, err
}
```

嵌入函数那里故意塞了一个「只会报错」的占位函数：所有写入都带预计算向量，真走到这一步说明调用方忘了先嵌入——报错比静默调用默认的 OpenAI 函数安全。

写入侧的键设计值得单独说。文档 ID 用「文件名#序号」做幂等键，metadata 带上 `file/head/seq` 三个字段——重复 ingest 同一块是覆盖更新，不是重复插入；而 metadata 是第 5 篇引用溯源的基础，检索命中后能回答读者「这句话出自哪篇哪节」：

```go
docs = append(docs, chromem.Document{
	ID:      c.File + "#" + strconv.Itoa(c.Seq), // 幂等键：重复 ingest 覆盖更新
	Content: c.Text,
	Embedding: vec, // 非空 → chromem-go 跳过嵌入函数，仅归一化
	Metadata: map[string]string{"file": c.File, "head": c.Head, "seq": ...},
})
```

查询侧走 `QueryEmbedding` 而不是 `Query`：查询向量我们自己算（同样的 bge-m3、同样的磁盘缓存），存储层只收向量。这里有个 v0.7.0 才修好的坑值得知道——早期版本要求调用方自己把查询向量归一化，不然相似度整体漂移；v0.7.0 的 changelog 里明确修了这个（PR #77，现在会自动检查并归一化）。版本号逐字抄 changelog，比背 API 可靠：

```go
// Search 用预计算的查询向量做 top-k 检索，filter 可按 file 等 metadata 等值过滤
func (s *Store) Search(ctx context.Context, queryVec []float64, topK int, filter map[string]string) ([]Result, error) {
	vec := make([]float32, len(queryVec)) // float64 → float32：存储层只吃 32 位
	for i, v := range queryVec {
		vec[i] = float32(v)
	}
	hits, err := s.col.QueryEmbedding(ctx, vec, topK, filter, nil)
	// 命中块还原出 Chunk 元数据：file/head/seq 原路返回，供引用溯源
	...
}
```

## 四、跑通全流程：ingest 一次，ask 随时

命令行工具两个，都在仓库 `rag/cmd/` 下。`ingest` 把 16 篇已发布教程灌进向量库（标题小节切分沿用第 3 篇实测最优策略），`ask` 加载向量库做 top-k 检索：

```bash
go run ./rag/cmd/ingest                 # 索引 10 篇 → 追加 glob 11 月的 6 篇
go run ./rag/cmd/ask "MCP server 怎么通过 HTTP 对外提供服务"
```

实测数据（2026-11-12，bge-m3，全部真实跑出来的）：16 篇文章切出 **260 块**，落盘 `.vecdb/` 目录只有 **1.1MB**；重复 ingest 块数纹丝不动——幂等键生效，磁盘缓存命中后第二次构建几乎不调 API。第一个问题「面试教练怎么判断回答太浅、决定追问」，Top1 相似度 0.645，**精确命中第 1 篇的「追问策略」整节**——第 3 篇标题切法的优势，在真实向量库上原样复现。

但第 3 篇埋的雷也原样复现了。第二个问题「MCP server 怎么通过 HTTP 对外提供服务」，把 top-k 放宽到 5，五个命中相似度挤在 0.701~0.677 之间，**正主（远程 MCP 那篇）依然缺席**，排在前面的全是「MCP 是什么」「client 骨架」这类概念性章节。分数挤成一团，就是典型的语义稀释——这是 top-k 调参救不了的病，得靠混合检索和重排序，第 6 篇专门问诊。

## 五、小结与预告

三个带走：选型先看数据量和运维预算，嵌入式方案够到万级以下；chromem-go 零依赖纯 Go，职责分离地接入——它管存取，你的 embedding 层管向量；文档 ID 和 metadata 第一天就按幂等和溯源设计，返工成本最低。

检索只是上半场。下一篇把它变成产品：**top-k 命中块怎么拼进 prompt、SSE 流式生成、以及让每一句回答都标注出处**——RAG 的完整问答链路，一次接通。

完整代码在 github.com/GO2AI-CN/ai-interview-coach 的 `rag/` 目录（store、ingest、ask 都在），欢迎 clone 下来跑一遍自己的语料。觉得这套「选型对比 + 实测数据」的写法有用，点赞、在看、转发，关注「Go2AI之路」，评论区聊聊你的向量库存了多大的数据。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
