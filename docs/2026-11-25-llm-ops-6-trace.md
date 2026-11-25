# LLM 应用工程化⑥：LLM 调用全链路 Trace，把黑盒拆开看

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。前五篇我们先后装了保险、度量、节流阀和缓存——但线上出问题时，你依然可能两眼一抹黑：这次请求走的是哪个 Prompt 版本？缓存命中了吗？钱花在哪个环节？本篇把黑盒拆开：**LLM 调用全链路 Trace**。

LLM 应用的故障排查有个传统软件没有的痛苦：**行为不确定**。同一个请求这次 2 秒下次 8 秒，这次对下次错。没有链路数据时，「这次为什么错了」在数学上不可回答——你只能猜。Trace 做的事就是把每一次请求的现场完整存档：经过了哪些环节、每个环节耗时多少、用了哪个 Prompt 版本、消耗多少 Token、命中缓存没有。

## 一、Trace 结构设计：先画图再埋点

好 Trace 的第一步是设计，不是代码。一条 LLM 问答请求的理想链路长这样：

```
HTTP POST /api/ask            (span: http handler, 属性: user_id)
├── embed                     (span: embedding, 属性: model, latency)
│   └── semantic_cache_lookup (event: hit=true, similarity=0.93)   ← 命中则终止
├── llm.chat                  (span: llm call)
│   ├── attributes: model, prompt_name, prompt_version,
│   │             input_tokens, output_tokens, cost_usd, finish_reason
│   ├── event: rate_limit_waited 120ms                       ← 第 4 篇限流等待
│   └── event: attempt=2 repair_after=schema_validation      ← 第 2 篇修复循环
├── output_validate           (span: 校验, 属性: passed=true)
└── cache_write               (event: ttl=24h)               ← 第 5 章回写
```

这个结构里藏着前面五篇的全部伏笔：Prompt 版本（①）、修复重试（②）、成本（④）、缓存命中（⑤）都在同一条链路上可见。**观测体系不是第七篇才加的，是把前六篇的埋点串成一条线。**

## 二、Go 实现：OTel 埋点

依赖（2026-11-25 核对最新版）：

```
require (
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0
)
```

### 2.1 TracerProvider 初始化

```go
package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// InitTracer 初始化 OTLP gRPC 导出器；本地开发把 endpoint 指向 Jaeger/Collector
func InitTracer(ctx context.Context, endpoint, serviceName string) (*sdktrace.TracerProvider, error) {
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
		)),
		// 采样策略：Head-based，开发期 AlwaysSample，生产 ParentBased(AlwaysOn)
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp, nil
}
```

### 2.2 LLM 调用的埋点封装

把埋点收敛到一个函数里，业务代码只调它——观测代码不污染业务逻辑的关键：

```go
import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("promptlab/llm")

// 自定义属性键：LLM 语义约定尚未完全稳定，用业务命名空间更可控
var (
	attrModel        = attribute.Key("llm.model")
	attrPromptName   = attribute.Key("llm.prompt.name")
	attrPromptVer    = attribute.Key("llm.prompt.version")
	attrInputTokens  = attribute.Key("llm.tokens.input")
	attrOutputTokens = attribute.Key("llm.tokens.output")
	attrCostUSD      = attribute.Key("llm.cost_usd")
	attrFinishReason = attribute.Key("llm.finish_reason")
)

type CallMeta struct {
	Model, PromptName, PromptVer string
}

// TraceLLM 包装一次 LLM 调用：自动记录模型、版本、Token、成本
// callFn 内部是真实调用（第 2 篇的 callModel）
func TraceLLM(ctx context.Context, meta CallMeta, callFn func(ctx context.Context) (usage Usage, err error)) error {
	ctx, span := tracer.Start(ctx, "llm.chat")
	defer span.End()
	span.SetAttributes(
		attrModel.String(meta.Model),
		attrPromptName.String(meta.PromptName),
		attrPromptVer.String(meta.PromptVer),
	)

	usage, err := callFn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetAttributes(
		attrInputTokens.Int64(usage.InputTokens),
		attrOutputTokens.Int64(usage.OutputTokens),
		attrCostUSD.Float64(usage.CostUSD),
		attrFinishReason.String(usage.FinishReason),
	)
	return nil
}
```

### 2.3 缓存命中与重试：事件而不是 Span

粒度选择有个实用原则：**异步/分支路径用 event，串行必经环节用 span**。缓存查找 0.5ms 就结束，为它开 Span 会让链路噪点爆炸；重试发生在 LLM span 内部，用 event 记录即可：

```go
// 在缓存查找处（第 5 篇 SemanticCache.Get 的调用侧）
span := trace.SpanFromContext(ctx)
if hit {
	span.AddEvent("semantic_cache_hit",
		trace.WithAttributes(
			attribute.Float64("cache.similarity", float64(sim)),
		))
	return cachedAnswer // 直接返回，llm.chat span 根本不会创建——链路图上「没有 LLM span」本身就是命中标志
}

// 在修复循环处（第 2 篇 Extract 内部）
span.AddEvent("llm.repair_attempt",
	trace.WithAttributes(
		attribute.Int("attempt", attempt),
		attribute.String("after_error", lastErr.Error()),
	))
```

### 2.4 HTTP 层自动埋点

`otelhttp` 一行接入，入口 Span 自动建立，下游 Span 靠 context 自然串联：

```go
import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

handler := otelhttp.NewHandler(mux, "http")
// http.ListenAndServe(addr, handler)
```

## 三、后端选型：数据送去哪

| 方案 | 特点 | 适用 |
| --- | --- | --- |
| **Langfuse**（开源，可自托管） | LLM 原生：Prompt 版本关联、Token/成本报表、评测集联动，和本系列前几篇的模型完全对得上 | 团队首选 |
| Jaeger / Tempo | 通用 Trace 后端，稳但 LLM 语义弱 | 已有可观测体系的团队 |
| OTel Collector + 自家栈 | 最灵活，工程量最大 | 平台型团队 |

无论选谁，OTel 埋点代码不用改——这正是埋点时只用标准 API、自定义属性走业务命名空间（2.2 节）的原因：语义约定（semantic conventions）在 LLM 领域尚未完全稳定，绑死某个版本的 `gen_ai.*` 约定，升级时会被迫全量改埋点。

## 四、Trace 驱动的三个真实场景

埋点完成之后，前面五篇的能力会自己长出新的化学反应：

1. **「这次为什么贵了？」** 按 `llm.prompt.version` + `llm.cost_usd` 聚合，一眼看出是某次 Prompt 膨胀导致输入 Token 均值涨了 40%——第 1 篇的版本纪律 + 第 4 篇的成本计量在链路上会师。
2. **「命中率怎么掉了？」** 第 5 篇提到 cache_hit_rate 异常：Trace 里捞几条 miss 链，看 `cache.similarity` 分布集中在 0.87~0.89——用户问题在变长，Embedding 相似度整体下移，该重新标定阈值了。
3. **「v3 Prompt 到底行不行？」** 第 3 篇的离线评测上线后，按 `llm.repair_attempt` 事件聚合：v3 的修复触发率是 v2 的 3 倍——离线评测没暴露的问题，生产 Trace 现形。回滚决策从「感觉」变成「事件率 3 倍」。

## 五、踩坑清单

1. **别把 Prompt 内容、用户输入写进 Span 属性**：PII 合规风险和存储成本都是真问题。需要时记录 hash 或长度，明文走独立审计日志。
2. **Token / 成本在 span 结束后也要可聚合**：放 attributes 而不是 event 的日志文本里——后者的检索和聚合体验很差。
3. **采样策略想清楚**：生产环境 100% 采样 LLM 链路通常承受得起（QPS 不高、Span 小），但全量 HTTP 链路采样 1%~10% 即可，用 ParentBased 让被采样的请求带全链路。
4. **流式响应的 Span 在流结束时才 End**：首个 chunk 到达不代表调用成功——在流消费完成后补录 `finish_reason` 和 usage。
5. **本地开发用 stdout 导出器**（`stdouttrace`），零依赖看链路；不要因为搭不起后端就放弃埋点。
6. **TraceID 写进业务日志**：`slog` 里加 TraceID 字段，日志和链路互相跳转，排障效率翻倍。

## 六、本篇小结与下篇预告

链路让系统从「能跑」变成「可看」：每个环节耗时、每分钱去向、每次修复重试，全部有档可查。最后两篇进入收官阶段——第 7 篇聊安全：**Prompt Injection 攻防**，这是 LLM 应用独有的、传统安全体系覆盖不到的攻击面。

---

## 版本说明（2026-11-25 核对）

| 依赖 | 本文使用版本 | 核对来源 |
| --- | --- | --- |
| go.opentelemetry.io/otel | v1.47.0（2026-10-02） | proxy.golang.org `@latest` |
| otelhttp（contrib） | v0.72.0（2026-10-02） | proxy.golang.org `@latest` |

## 相关阅读

- [LLM 应用工程化④：Token 成本计量与限流](https://blog.911015.com/ai/llm-ops-4-cost-ratelimit.html) —— `llm.cost_usd` 属性与成本计量器同源
- [LLM 应用工程化⑤：语义缓存实战](https://blog.911015.com/ai/llm-ops-5-semantic-cache.html) —— cache 事件设计承接本篇 2.3 节
- [LLM 应用工程化③：自动化评测集](https://blog.911015.com/ai/llm-ops-3-eval-suite.html) —— 离线评测与生产 Trace 互相印证
- [LLM 应用工程化系列聚合目录](https://blog.911015.com/ai/llm-ops.html)

> **声明**：本文由 AI 辅助生成，API 均对照官方文档核验；如发现任何错误或不一致，欢迎在评论区指正。
