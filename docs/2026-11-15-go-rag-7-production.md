# 只重建改动的文件：Go RAG 增量索引与 Docker 部署

知识库上线第二天，你发现某篇文档有个错别字。改完想同步进向量库——难道 357 个块全部重新嵌入一遍？再过几天删掉一篇旧文，它的块还躺在库里被检索出来，读者点开出处，来源文件却没了。

demo 和生产的差距，就藏在这些「日常改动」里。这篇给系列第 5 篇的问答助手补上生产化三件套：增量索引、变更检测、metadata 过滤，最后打成 Docker 镜像交付。全部代码开源在 github.com/GO2AI-CN/ai-interview-coach，数据全部真机实测。

## 一、先算账：全量重建贵在哪

我们的语料是 22 篇教程、357 个块。用第 4 篇的 ingest 维护它，每改一个字都要全库重切、重嵌、重写，账单有三笔：

- **钱**：embedding API 按块计费。好在第 2 篇做了磁盘缓存，块文本没变就命中缓存不花钱——但靠缓存兜底是运气，不是设计；
- **时间**：全量流程再快也是全量，语料涨到几千块时，改一行字等几分钟；
- **正确性**：这是最疼的一笔。文件删了，ingest 可不知道，旧块永远留在库里，检索照出、引用照给——幽灵出处。

解法是给它记一笔账。

## 二、增量索引：给每个文件记一笔账

新命令 `rag/cmd/sync` 维护一本账本（向量库目录下的 `manifest.json`）：每个已索引文件记一条「内容哈希 + 块 ID 清单」。每次运行重新计算磁盘文件哈希，两相对比，文件分成三类：

```go
// Plan 对比磁盘现状（file→hash）与账本，把文件分成三类
func (m *Manifest) Plan(disk map[string]string) (rebuild, deleted, unchanged []string) {
	for f, h := range disk {
		if st, ok := m.Files[f]; ok && st.Hash == h {
			unchanged = append(unchanged, f) // 哈希一致：整块跳过
		} else {
			rebuild = append(rebuild, f) // 新增或已变更：只重建它
		}
	}
	for f := range m.Files {
		if _, ok := disk[f]; !ok {
			deleted = append(deleted, f) // 账上有、盘上无：清残块
		}
	}
	return rebuild, deleted, unchanged
}
```

账本只记两个字段，是刻意为之：哈希回答「内容变没变」，ID 清单回答「删的时候删哪些」——多了都是负担。账本就放在向量库目录里，和 chromem-go 的持久化文件同进同出，备份迁移整个目录拷走即可；落盘时先写临时文件再改名，进程半路被杀也不会留下半本坏账。没动的文件连切分都不做，零嵌入零写入。实测（Go 1.26.0，chromem-go v0.7.0）：

| 运行 | 结果 | 重建块 | 缓存命中 | 回源 API | 耗时 |
| --- | --- | --- | --- | --- | --- |
| 首次（建账） | 22 篇全部重建 | 357 | 260 | 97 | 1.9s |
| 紧接着第二次 | 22 篇全未变 | 0 | — | 0 | 0.1s |

首次运行账本不存在，22 篇全部重建——但旧库 260 块的向量早就在缓存里，一分钱没花，真正回源的只有 RAG 系列 6 篇新文章的 97 块。第二次开始才是日常态：0.1 秒完成全库体检。这张表值得收藏，它就是「增量」二字的全部含义。

## 三、变更检测实测：改一行，只花一块的钱

光说不练是玄学，直接做对照实验。拿两篇文章建个演示库（32 块），然后分别模拟「改内容」和「删文件」：

| 操作 | sync 检测结果 | 重建块 | 回源 API | 库内块数 |
| --- | --- | --- | --- | --- |
| 初始同步 2 篇 | 2 篇新增 | 32 | 0（全缓存命中） | 32 |
| 给其中一篇追加一行 | 1 篇变更、1 篇未变 | 16 | **1** | 32 |
| 删掉其中一篇 | 1 篇已删除 | 0 | 0 | **16** |

追加一行那次最说明问题：变更文件重切出 16 块，其中 15 块文本没变、缓存白捡，只有包含新行的那 1 块真正调了 API——改一行就只花一块的钱。删文件那次，sync 按账本里的 ID 清单把 16 个残块逐条删净，库内块数 32 回落到 16：

```go
for _, f := range rebuild {
	if old, ok := m.Files[f]; ok {
		st.Delete(ctx, old.IDs...) // 先删旧块：新切块数变少也不留残块
	}
	cs := chunk.TagSeries(chunk.TagFile(f, chunk.ByHeading(string(contents[f]), 300)))
	newIDs[f] = index.IDsFor(f, len(cs))
	chunks = append(chunks, cs...)
}
```

「先删旧块再写新块」这个顺序不能反——文章改短后块数变少，不先删，多出来的旧 ID 就成了漏网残块。还有一点容易忽略：删除按的是账本里的 ID 清单逐条删，而不是按文件名模糊扫库。前者删得精准，同名文件重建多少次都不怕误伤；后者一旦 ID 约定变了，删不干净还没人报警。

## 四、metadata 过滤：检索前先圈定范围

库里的文章已经横跨三个系列（面试教练 / MCP / RAG），问题一跨界就容易串台。第 6 篇靠混合检索救召回，这篇换个更简单的思路：**入库时给每块打上系列标签，检索时先过滤再排序**。

```go
// TagSeries 给一组 chunk 打上系列标签，检索时即可按 series 过滤
func TagSeries(cs []Chunk) []Chunk {
	for i := range cs {
		cs[i].Series = SeriesOf(cs[i].File) // 按文件名约定推断 coach/mcp/rag
	}
	return cs
}
```

标签随 sync 写入 chromem-go 的 metadata，`ask` 加个 `-series` 参数走 where 过滤。同一问题「怎么把面试历史库包装成 MCP 服务？」实测：

| 检索方式 | top-3 命中（相似度） |
| --- | --- |
| 不过滤 | mcp-5（0.72）、mcp-4（0.70）、**rag-6（0.69）** |
| `-series mcp` | mcp-5（0.72）、mcp-4（0.70）、mcp-3（0.67），全在系列内 |
| `-series rag` | rag-6（0.69）、rag-1（0.67）、rag-6（0.62），同样纯净 |

不过滤时第三名混进了一篇 RAG 文章——0.69 的相似度跟正主咬得很紧，纯靠向量分不清。一行过滤条件，噪音整块消失。过滤还有个白捡的收益：候选集从全库 357 块缩到单系列一百来块，库越大，这一步省下的比对越多。按系列过滤只是起点，日期、作者、文档类型都能照这个路子打标签——过滤条件拼起来，就是知识库的权限与租户边界。

## 五、Docker 部署：一个静态二进制走天下

交互式 CLI 没法部署，先把问答链路包成 HTTP 服务 `rag/cmd/serve`：`GET /healthz` 给容器健康检查，`POST /ask` 收问题返回答案和引用清单。服务本身无状态——检索库是只读挂载、会话不落地——想扛量就在前面挂负载均衡多开几个实例。实测（DeepSeek 端点）：healthz 返回 200 仅 1.6ms；POST 一个真实问题，5.5 秒拿回带 `[1][2]` 标注的正确答案和三条出处，大头全在 LLM 生成上。

部署用多阶段构建，全部依赖纯 Go 无 CGO，静态编译一个二进制就够：

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ragserve ./rag/cmd/serve

FROM alpine:3
COPY --from=build /out/ragserve /usr/local/bin/ragserve
EXPOSE 8080
ENTRYPOINT ["ragserve"]
```

两个镜像标签都核对过官方清单：`golang:1.26-alpine` 与 go.mod 的 go 1.26 对齐，`alpine:3` 是官方滚动标签。本机用同样的 `CGO_ENABLED=0` 交叉编译出 Linux 静态 ELF，**28.1 MB**——Dockerfile 里那行构建命令就是它，可行性已验证。配套 `.dockerignore` 把 `.env`、`.vecdb`、`.embed-cache` 挡在构建上下文外，凭证绝不进镜像。向量库和缓存走 volume 挂载，一条命令起服务：

```bash
docker build -t rag-serve .
docker run -p 8080:8080 --env-file .env \
  -v "$PWD/.vecdb:/data/.vecdb" -v "$PWD/.embed-cache:/data/.embed-cache" \
  rag-serve -db /data/.vecdb -cache /data/.embed-cache
```

## 六、小结与预告

生产化清单，拿走对照：

- [ ] 增量索引：文件哈希记账，只重建改动的文件
- [ ] 变更检测：删文件清残块，改文件先删后写
- [ ] metadata 过滤：入库打标签，检索先圈范围
- [ ] 部署交付：静态二进制 + 多阶段镜像，凭证不进镜像

下一篇是本系列收官：把主流向量库 Milvus、Qdrant、Weaviate、pgvector、sqlite-vec 和 chromem-go 拉到一起做横评，聊聊 Agentic RAG 与 GraphRAG 的风口，再把整个仓库八篇的代码家底盘点一遍。

如果这个系列对你有帮助，欢迎**点赞、在看、转发**，关注「Go2AI之路」，收官篇不见不散。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
