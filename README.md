# AI 模拟面试教练（Go + 大模型）

> ⭐ **项目已开源**：[github.com/GO2AI-CN/ai-interview-coach](https://github.com/GO2AI-CN/ai-interview-coach)　代码 MIT 协议，欢迎 star / fork / PR，公众号「Go2AI之路」配套教程随仓库同步更新。

用 Go 从零实现的产品级面试 Agent：**输入目标岗位（可选简历 PDF）→ 生成面试计划 → 逐题提问 → 评估回答深浅 → 答得浅就追问 → 面试结束输出结构化评分报告**。

配套教程见 [`docs/`](docs/)：

- [上篇：出题、追问、打分，一个 Agent 全搞定](docs/2026-10-27-ai-interview-coach-1.md)
- [下篇：接入简历解析，让面试官先读你的简历](docs/2026-10-28-ai-interview-coach-2.md)
- [第三篇：Web 版 + SSE 流式输出](docs/2026-10-28-ai-interview-coach-3-web.md)
- [第四篇：接入飞书机器人（长连接，零公网 IP）](docs/2026-10-28-ai-interview-coach-4-feishu.md)
- [第五篇：语音作答（ASR）+ 网页版语音彩蛋](docs/2026-10-28-ai-interview-coach-5-voice.md)
- [第六篇：部署 7×24——systemd 守护与成本监控](docs/2026-10-29-ai-interview-coach-6-deploy.md)
- [第七篇：面试历史落库（SQLite）——「历史」指令回看既往报告](docs/2026-10-31-ai-interview-coach-7-sqlite.md)
- [第八篇·收官：架构回顾与「fork 改造成你的 Agent」路线图](docs/2026-10-31-ai-interview-coach-8-finale.md)

第二系列「MCP 协议与 Agent 工程化」（预告：把面试教练的 SQLite 历史库包装成 MCP 服务）：

- [系列第 1 篇：为什么大模型需要 MCP？一篇讲透 Agent 工具的 USB-C 接口](docs/2026-10-31-go-mcp-1-intro.md)
- [系列第 2 篇：不写一行 SDK，200 行 Go 手写 MCP Server 全解析](docs/2026-10-31-go-mcp-2-minimal-server.md)
- [系列第 3 篇：MCP 进阶：Tools 与 Resources 实战](docs/2026-11-02-go-mcp-3-tools-resources.md)
- [系列第 4 篇：把面试教练的历史库包装成 MCP 服务（上）](docs/2026-11-02-go-mcp-4-coachstore-server.md)
- [系列第 5 篇：MCP 函数调用循环：让模型自己决定调工具](docs/2026-11-02-go-mcp-5-client-loop.md)
- [系列第 6 篇：Sampling：让 server 反向请求你的大模型](docs/2026-11-02-go-mcp-6-prompts-sampling-roots.md)
- [系列第 7 篇：远程 MCP 实战：HTTP/SSE 传输与鉴权全解析](docs/2026-11-04-go-mcp-7-remote-http.md)
- [系列第 8 篇·收官：官方 SDK、社区库与服务器注册表全景](docs/2026-11-05-go-mcp-8-finale.md)

第三系列「RAG 与向量检索」（实战项目：把本仓库 docs/ 16 篇文章 ingest 成带引用出处的 Go 文档问答助手）：

- [系列第 1 篇：大模型为什么总胡说八道？RAG 实战第一讲](docs/2026-11-06-go-rag-1-intro.md)
- [系列第 2 篇：向量 Embedding 实战：Go 调用与模型选型](docs/2026-11-10-go-rag-2-embedding-api.md)
- [系列第 3 篇：RAG 切分实战：chunk 怎么切，检索才不翻车](docs/2026-11-11-go-rag-3-chunking.md)
- [系列第 4 篇：向量库存哪？四类方案对比与 chromem-go 实战](docs/2026-11-12-go-rag-4-vector-store.md)
- [系列第 5 篇：让 AI 回答有出处：Go 流式问答全链路实战](docs/2026-11-12-go-rag-5-qa-chain.md)
- [系列第 6 篇：10 道题实测：BM25+RRF 混合检索救回 RAG 翻车题](docs/2026-11-12-go-rag-6-hybrid-search.md)

## 功能

- **面试计划**：基于岗位能力模型（rubric）一次性生成 5~8 题，每题带可验证的考察要点（checkpoints）
- **追问策略**：置信度 + 关键点覆盖度两把尺子判断回答深浅，同一题最多追问 2 轮，追问有方向（基于缺失要点）
- **结构化输出**：评分报告用 JSON Schema（Structured Outputs）锁死格式，Go struct 解析并渲染成 Markdown
- **简历解析**：PDF 文本提取（纯 Go，无 CGO）→ 模型生成结构化摘要 → 生成针对性简历深挖题
- **多轮记忆**：面试状态持久化到 `.coach-state.json`，今天面 3 题，明天 `coach continue` 接着面
- **网页版**：SSE 流式点评，逐字打出评估（见第三篇）
- **飞书机器人**：官方长连接接入，群里 @ 机器人即可开考，无需公网 IP（见第四篇）
- **语音作答**：飞书语音消息自动转文字后走同一评估管线，文字/语音可混答（见第五篇）
- **成本可见**：每场面试结束报告末尾附 LLM token 消耗（输入/输出分列），配合 systemd unit 可 7×24 部署（见第六篇）
- **面试历史**：每场问答、评估、报告落库 SQLite（纯 Go，无 CGO），飞书发「历史」回看最近场次、「历史 N」调取完整报告（见第七篇）

## 快速开始

要求 Go 1.26+（openai-go v3 的硬性要求；Go 1.25 请把 SDK 固定到 v3.74.0）。

```bash
go mod download

export OPENAI_API_KEY=你的Key
export COACH_MODEL=kimi-k2            # 可选，默认 gpt-4o-mini
export OPENAI_BASE_URL=兼容端点       # 可选，DeepSeek/Kimi 等 OpenAI 兼容端点

# 方式一：只给岗位
go run ./cmd/coach new --pos "高级后端工程师"

# 方式二：给岗位 + 简历 PDF，面试官先读简历再出题
go run ./cmd/coach new --pos "高级后端工程师" --resume ./resume.pdf

# 面试没面完，下次接着面
go run ./cmd/coach continue

# 启动网页版（SSE 流式点评，浏览器打开即用）
go run ./cmd/coach web --addr :8080

# 启动飞书机器人（群里 @ 机器人：面试 <岗位>）
export FEISHU_APP_ID=你的AppID
export FEISHU_APP_SECRET=你的AppSecret
go run ./cmd/coach bot

# 语音作答（OpenAI 兼容 ASR，默认 SiliconFlow 免费模型）
export OPENAI_BASE_URL=https://api.siliconflow.cn/v1   # 或其他兼容端点
export OPENAI_API_KEY=你的Key
export COACH_ASR_MODEL=FunAudioLLM/SenseVoiceSmall      # 可选

# 面试历史落库（飞书「历史」指令；不设则关闭）
export COACH_DB_PATH=./coach.db
```

面试结束后在当前目录生成 `report.md`（各维度得分 + 薄弱点 + 改进建议）。

## 工程结构

```
cmd/coach/        命令行入口（new / continue / web / bot 四个子命令）
internal/coach/   面试状态机：计划 / 评估 / 追问判断 / 状态持久化
internal/llm/     openai-go v3 封装（JSON Schema 模式 + 纯文本 + 流式 + token 用量统计）
internal/resume/  PDF 文本提取 + 简历结构化摘要
internal/report/  评分报告 Markdown 渲染
internal/web/     网页版 HTTP 服务（SSE 流式）
internal/bot/     飞书机器人：长连接接入 + 会话路由 + 语音消息（IM 适配层）
internal/asr/     语音识别：OpenAI 兼容 /v1/audio/transcriptions 封装
internal/store/   面试历史落库：SQLite（modernc.org/sqlite，纯 Go 无 CGO）
deploy/           systemd unit 文件（7×24 部署，见第六篇）
mcp/minimal/      第二系列：不依赖 SDK 的最小 MCP Server（JSON-RPC 2.0 + stdio）
mcp/toolsdemo/    第二系列：进阶版——工具注册表 + 参数校验 + Resources 资料库
mcp/coachstore/   第二系列：面试历史 SQLite 库的只读 MCP 服务（query_only 物理禁写）
mcp/coachstore-sdk/ 第二系列：官方 go-sdk v1.8.0 版服务端（schema 由结构体推导，与手写版互通）
mcp/coachclient/  第二系列：手写 MCP client + 函数调用循环（LLM 接口可插拔）
mcp/coachclient-sdk/ 第二系列：用官方 go-sdk v1.8.0 重写的 client（调用面与手写版一致，可对照）
mcp/extras/       第二系列：双向请求演示——Prompts / Sampling（已废弃，仅教学）/ Roots
mcp/remote/       第二系列：Streamable HTTP 传输（POST/GET + SSE + 会话与鉴权）
rag/cmd/embeddemo/ 第三系列：零依赖教学 demo——字符 bigram 手写 embedding + 暴力余弦检索
rag/embedding/    第三系列：OpenAI 兼容 /v1/embeddings 薄封装（批量 + 磁盘缓存 + 余弦相似度）
rag/cmd/embedapi/ 第三系列：真实 Embedding API 语义检索实测（bge-m3，含缓存对比）
rag/chunk/        第三系列：三种切分策略（固定窗口/段落合并/标题小节）+ chunk 元数据
rag/cmd/chunkdemo/ 第三系列：16 篇教程三策略切分检索对比实测
rag/store/        第三系列：chromem-go 存储层（持久化/幂等写入/top-k 检索/metadata 过滤）
rag/cmd/ingest/   第三系列：索引构建 CLI（docs → 标题切分 → bge-m3 嵌入 → 向量库）
rag/cmd/ask/      第三系列：检索验证 CLI（top-k 命中块 + 来源 + 相似度）
rag/prompt/       第三系列：检索结果组装层（资料编号注入 + 引用溯源清单，不依赖 SDK）
rag/cmd/chat/     第三系列：完整问答 CLI（检索 → 拼 prompt → SSE 流式 → [n] 溯源，多轮）
rag/hybrid/       第三系列：混合检索——BM25 关键词倒排（中文 bigram 切词）+ RRF 名次融合
rag/cmd/eval/     第三系列：检索质量评测 CLI（10 道考题 × 三路检索，hit@k 汇总）
rag/cmd/rerankdemo/ 第三系列：LLM 重排序演示（RRF top-10 → 大模型逐块打分重排）
docs/             配套公众号教程
```

## 测试

无需 API Key，Mock 客户端驱动状态机全链路：

```bash
go test ./...
```

## 设计要点

详见 docs/ 下两篇教程。核心三件事：

1. **先建 rubric 再出题**：checkpoints 提前从模型嘴里"套"出来，作为追问判断的依据
2. **两把尺子决定追问**：`confidence < 3` 或 `coverage < 0.6` 必追问，每题最多 2 轮防套娃
3. **Schema 锁死输出**：`response_format` + `strict: true`，杜绝评分报告的格式漂移

## License

[MIT](LICENSE) — 代码随意用，抄作业注明出处即可；`docs/` 下的公众号教程转载需联系作者授权。
