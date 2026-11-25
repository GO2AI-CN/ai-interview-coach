// Package obs 实现《LLM 应用工程化》系列第 6 篇的 OpenTelemetry 埋点。
package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// InitTracer 初始化 OTLP gRPC 导出器；本地开发把 endpoint 指向 Collector / Jaeger
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
		// 开发期 AlwaysSample；生产用 ParentBased(AlwaysOn) 并给 HTTP 层加比例采样
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp, nil
}

var tracer = otel.Tracer("ai-interview-coach/llmops")

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

// SpanMeta 一次 LLM 调用的埋点元数据
type SpanMeta struct {
	Model, PromptName, PromptVer string
}

// StartSpan 开启业务子 Span
func StartSpan(ctx context.Context, name string, meta SpanMeta) (context.Context, trace.Span) {
	ctx, span := tracer.Start(ctx, name)
	span.SetAttributes(
		attrModel.String(meta.Model),
		attrPromptName.String(meta.PromptName),
		attrPromptVer.String(meta.PromptVer),
	)
	return ctx, span
}

// RecordUsage 记录 Token 与成本到当前 Span
func RecordUsage(ctx context.Context, inputTokens, outputTokens int64, costUSD float64, finishReason string) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attrInputTokens.Int64(inputTokens),
		attrOutputTokens.Int64(outputTokens),
		attrCostUSD.Float64(costUSD),
		attrFinishReason.String(finishReason),
	)
}

// RecordCacheHit 缓存命中事件（命中时链路里没有 LLM Span，此事件即命中标志）
func RecordCacheHit(ctx context.Context, similarity float32) {
	trace.SpanFromContext(ctx).AddEvent("semantic_cache_hit",
		trace.WithAttributes(attribute.Float64("cache.similarity", float64(similarity))))
}

// RecordRepairAttempt 修复循环重试事件
func RecordRepairAttempt(ctx context.Context, attempt int, lastErr error) {
	trace.SpanFromContext(ctx).AddEvent("llm.repair_attempt",
		trace.WithAttributes(
			attribute.Int("attempt", attempt),
			attribute.String("after_error", lastErr.Error()),
		))
}

// RecordInjection 注入检测命中事件（第 7 篇，进告警与样本集）
func RecordInjection(ctx context.Context, matches []string) {
	span := trace.SpanFromContext(ctx)
	attrs := make([]attribute.KeyValue, len(matches))
	for i, m := range matches {
		attrs[i] = attribute.String("match", m)
	}
	span.AddEvent("injection_detected", trace.WithAttributes(attrs...))
	span.SetStatus(codes.Error, "injection detected")
}
