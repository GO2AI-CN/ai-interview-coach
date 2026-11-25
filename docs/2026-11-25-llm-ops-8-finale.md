# LLM 应用工程化⑧（收官）：把 7 个组件拼成可复用的工程化平台

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。这是系列第 8 篇，收官。前 7 篇每篇解决一个问题，各自独立成文；本篇做一件只有系列结尾才值得做的事——**把它们拼成一个整体**，让这套东西从「七个知识点」变成「一个你能搬走的平台」。

## 一、回顾：七个组件，一张全景图

先把 7 篇的产出摆在一起看：

| 篇 | 组件 | 一句话职责 |
| --- | --- | --- |
| ① | Prompt Registry | 版本化、灰度、回滚 |
| ② | 结构化防线 | 输出协议约束 + 修复循环 |
| ③ | 评测集 | 回归门禁，版本对比 |
| ④ | 成本计量 | 记账、熔断、限流 |
| ⑤ | 语义缓存 | 相似问题零成本复用 |
| ⑥ | Trace | 全链路留痕与聚合分析 |
| ⑦ | 安全护栏 | 注入检测、工具最小权限、输出消毒 |

这 7 个组件单看都是「可以晚点再做」的东西——合起来才是 LLM 应用和 demo 的分界线。它们的装配关系：

```
                    ┌─────────────────────────────────────┐
   请求 → ⑦安全护栏 → │            LLMService                │
                    │                                      │
                    │  ⑤语义缓存 ──miss──→ ④成本钩子       │
                    │        │hit              │           │
                    │        ↓                ↓           │
                    │     直接返回      ①Registry 取 Prompt │
                    │                       │              │
                    │                       ↓              │
                    │               ⑥Trace span 包裹       │
                    │                       │              │
                    │                       ↓              │
                    │               外部 LLM API           │
                    │                       │              │
                    │                       ↓              │
                    │               ②结构化防线解析        │
                    │                       │              │
                    │              ⑦输出消毒 → 返回        │
                    └─────────────────────────────────────┘
        ③评测集：CI 里对 ①的每个版本回归评测
        ⑥Trace：所有环节的日志、事件、指标出口
```

## 二、平台代码结构：llmops 模块

7 篇的代码片段在文章里是线性的，在仓库里按组件分目录（已整合进开源工程 [GO2AI-CN/ai-interview-coach](https://github.com/GO2AI-CN/ai-interview-coach) 的 `llmops/` 目录，复用项目已有的 LLM 抽象）：

```
llmops/
├── prompts/            # ① Prompt 资产（embed 进二进制）
│   └── customer-service/v1.json v2.json v3.json
├── registry/           # ① Registry：加载、灰度、决策日志
│   └── registry.go
├── guard/              # ② 结构化防线 + ⑦注入检测与消毒
│   ├── extract.go      #    Structured Outputs + 修复循环
│   ├── injection.go    #    注入检测中间件
│   └── sanitize.go     #    输出消毒
├── eval/               # ③ 评测集运行器（go test 入口）
│   ├── runner.go
│   └── eval_test.go
├── evals/
│   └── dataset.jsonl   # ③ 黄金数据集（含注入样本）
├── cost/               # ④ 计量、预算熔断、限流
│   ├── meter.go
│   └── limiter.go
├── cache/              # ⑤ 语义缓存
│   └── semantic.go
├── obs/                # ⑥ OTel 初始化与埋点封装
│   └── tracer.go
├── service/            # 总装配：LLMService 门面
│   └── service.go
├── cmd/llmops-demo/main.go（离线 Mock 全链路演示）
└── go.mod
```

## 三、总装配：一个门面接口

平台对业务代码只暴露一个入口。业务方不需要知道下面有 7 个组件——这正是「平台」和「代码堆」的区别：

```go
package service

// LLMService 业务侧唯一入口
type LLMService struct {
	Reg      *registry.Registry
	Cache    *cache.SemanticCache
	Meter    *cost.Meter
	Detector *guard.InjectionDetector
	Client   *openai.Client

	Model       string
	PromptName  string
	PromptVer   string // "latest" 或钉死版本
	CacheTTL    time.Duration
}

// Analyze 一次完整的业务调用：护栏 → 缓存 → 成本 → LLM → 防线 → 消毒
func (s *LLMService) Analyze(ctx context.Context, userID, ticket string) (*guard.Review, error) {
	ctx, span := obs.StartLLMSpan(ctx, s.PromptName, s.PromptVer)
	defer span.End()

	// ⑦ 安全护栏（第 7 篇）
	if r := s.Detector.Scan(ticket); r.Hit {
		obs.RecordInjection(ctx, r.Matches)
		return nil, fmt.Errorf("输入包含可疑指令内容")
	}

	// ⑤ 语义缓存（第 5 篇）
	emb, _ := s.embed(ctx, ticket)
	if e, sim, hit := s.Cache.Get(emb, s.Model, s.PromptVer); hit {
		obs.RecordCacheHit(ctx, sim)
		return guard.DecodeReview(e.Answer)
	}

	// ④ 成本与限流（第 4 篇）
	if !s.Meter.Allow(s.Model) {
		return nil, fmt.Errorf("daily budget exhausted")
	}

	// ① Prompt 解析（第 1 篇）+ ⑥ Trace（第 6 篇）+ ② 结构化防线（第 2 篇）
	prompt, _ := s.Reg.Get(s.PromptName, s.PromptVer)
	review, usage, err := guard.ExtractTraced(ctx, s.Client, prompt.Render(...), ticket)
	if err != nil {
		return nil, err
	}
	s.Meter.Record(s.Model, usage)          // ④ 记账
	obs.RecordUsage(ctx, usage)             // ⑥ Span 属性

	// ⑦ 输出消毒 + ⑤ 回写缓存
	answer := guard.SanitizeOutput(guard.EncodeReview(review))
	s.Cache.Set( /* ... 第 5 篇 Entry ... */ )
	return review, nil
}
```

`ExtractTraced` 是第 2 篇 `Extract` 和第 6 篇 `TraceLLM` 的组合——修复循环的每一次 `repair_attempt` 事件都会落进同一个 LLM span。

## 四、运行视图：一个请求的完整旅程

把前面几篇的所有钩子按时间序排一遍，你会得到一个漂亮的「平台自检清单」——线上任何异常，先问这份清单的哪一环没响：

1. **⑦ 注入检测**：命中即拒绝，事件进 Trace；
2. **⑤ 缓存检索**：命中即返回（Span 里没有 LLM 调用就是命中标志）；
3. **④ 预算熔断**：当日额度耗尽直接降级或拒绝；
4. **① Prompt 解析**：版本来自 Registry，决策日志留痕；
5. **⑥ LLM Span**：模型、版本、Token、成本、finish_reason 全记录；修复重试以事件形式内嵌；
6. **② 结构化防线**：schema + 业务双校验，失败进修复循环（上限 3 次）；
7. **⑦ 输出消毒**：链接白名单过滤；
8. **⑤ 缓存回写**：TTL + 版本隔离。

## 五、迁移路线图：把它变成你的

这套平台最好的用法不是照抄，而是**按业务水位渐进 adoption**。四阶段路线：

**第 1 周——地基**：搬 ①④⑥（Registry + 成本 + Trace）。三者代码量最小、收益最直接：先有版本纪律和成本/链路可见性，后面所有决策才有依据。

**第 2 周——稳定**：加 ②⑦（结构化防线 + 安全护栏）。上线即带修复循环和注入检测，badcase 修复周期从「玄学排查」缩到「看 Trace」。

**第 3 周——效率**：加 ⑤（语义缓存）。注意顺序特意放后面——**缓存会掩盖问题，先让系统透明，再让它快**。

**第 4 周——质量闭环**：加 ③（评测集）。此时你已有一周的真实日志：抽 100 条 badcase、50 条典型请求，固化成第一批数据集；此后每次 Prompt 改动、模型升级、缓存策略调整，都过一遍门禁。

每一步都可独立回退，每一步的收益都不依赖下一步——这是刻意的设计：adoption 顺序即风险顺序，从最不可能出错的地方开始。

## 六、未完结的部分

诚实地说，这个系列结束时你拥有的仍是一个「最小完整」的平台。生产化还有几个本文没展开、但值得列在 backlog 的硬骨头：

- **多轮对话状态管理**：会话摘要、上下文压缩、跨轮缓存一致性；
- **模型路由**：按任务难度/成本动态选模型（比固定降级链更省）；
- **评测集的持续运营**：线上 badcase → 用例的半自动化流水线；
- **Prompt 资产的团队协作**：review 流程、生效审批、跨环境（dev/staging/prod）发布。

这些每个都能再开一个系列——如果有你想优先看的，后台留言告诉我们。

## 七、结语

这个系列从一个具体的痛点出发：demo 很惊艳，上线很狼狈。7 篇之后，狼狈的部分应该都有了着落——Prompt 有版本，输出有保险，质量有度量，成本有阀门，重复请求有缓存，线上行为有留痕，攻击面有护栏。

它们每一个单拎出来都不性感，但工程化本来就是这样：**不性感的东西，决定了系统能活多久**。感谢跟到这里。代码已随 [ai-interview-coach](https://github.com/GO2AI-CN/ai-interview-coach) 的 `llmops/` 目录开源，后台回复「工程化」获取系列合集与更新通知。

---

## 系列目录

| # | 篇目 | 链接 |
| --- | --- | --- |
| 1 | Prompt 管理别再硬编码 | [→](https://blog.911015.com/ai/llm-ops-1-prompt-mgmt.html) |
| 2 | 结构化输出与 Schema 校验 | [→](https://blog.911015.com/ai/llm-ops-2-structured-output.html) |
| 3 | 自动化评测集 | [→](https://blog.911015.com/ai/llm-ops-3-eval-suite.html) |
| 4 | Token 成本计量与限流 | [→](https://blog.911015.com/ai/llm-ops-4-cost-ratelimit.html) |
| 5 | 语义缓存实战 | [→](https://blog.911015.com/ai/llm-ops-5-semantic-cache.html) |
| 6 | LLM 调用全链路 Trace | [→](https://blog.911015.com/ai/llm-ops-6-trace.html) |
| 7 | Prompt Injection 攻防 | [→](https://blog.911015.com/ai/llm-ops-7-injection.html) |
| 8 | 工程化平台收官（本篇） | [→](https://blog.911015.com/ai/llm-ops-8-finale.html) |

**[系列聚合目录（持续维护）→](https://blog.911015.com/ai/llm-ops.html)**

## 相关阅读

- [Go RAG 实战 - 系列聚合目录](https://blog.911015.com/ai/go-rag.html) —— 同方法论的另一条主线：从原理到生产化
- [Go 手写 MCP 实战 - 系列聚合目录](https://blog.911015.com/ai/go-mcp.html) —— 平台工具侧的协议基础
- [AI 模拟面试教练专栏](https://blog.911015.com/coach/) —— 本系列组件的实践宿主项目

> **声明**：本文由 AI 辅助生成，全文结构与代码承接本系列前 7 篇；如发现任何错误或不一致，欢迎在评论区指正。
