# AI 模拟面试教练（Go + 大模型）

用 Go 从零实现的产品级面试 Agent：**输入目标岗位（可选简历 PDF）→ 生成面试计划 → 逐题提问 → 评估回答深浅 → 答得浅就追问 → 面试结束输出结构化评分报告**。

配套教程见 [`docs/`](docs/)：

- [上篇：出题、追问、打分，一个 Agent 全搞定](docs/2026-10-27-ai-interview-coach-1.md)
- [下篇：接入简历解析，让面试官先读你的简历](docs/2026-10-28-ai-interview-coach-2.md)

## 功能

- **面试计划**：基于岗位能力模型（rubric）一次性生成 5~8 题，每题带可验证的考察要点（checkpoints）
- **追问策略**：置信度 + 关键点覆盖度两把尺子判断回答深浅，同一题最多追问 2 轮，追问有方向（基于缺失要点）
- **结构化输出**：评分报告用 JSON Schema（Structured Outputs）锁死格式，Go struct 解析并渲染成 Markdown
- **简历解析**：PDF 文本提取（纯 Go，无 CGO）→ 模型生成结构化摘要 → 生成针对性简历深挖题
- **多轮记忆**：面试状态持久化到 `.coach-state.json`，今天面 3 题，明天 `coach continue` 接着面

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
```

面试结束后在当前目录生成 `report.md`（各维度得分 + 薄弱点 + 改进建议）。

## 工程结构

```
cmd/coach/        命令行入口（交互问答循环）
internal/coach/   面试状态机：计划 / 评估 / 追问判断 / 状态持久化
internal/llm/     openai-go v3 封装（JSON Schema 模式 + 纯文本模式）
internal/resume/  PDF 文本提取 + 简历结构化摘要
internal/report/  评分报告 Markdown 渲染
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

MIT
