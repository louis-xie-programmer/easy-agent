package agent

import (
	"io"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// tracer 是 OpenTelemetry 的 Tracer 实例，用于创建和管理 Span
var tracer trace.Tracer

// init 函数在包加载时执行，用于初始化 tracer 实例
func init() {
	// 获取一个命名为 "easy-agent/agent" 的 Tracer
	tracer = otel.Tracer("easy-agent/agent")
}

// InitTracerProvider 初始化 OpenTelemetry Tracer Provider
// version: 应用程序的版本号，用于服务资源属性
// 返回配置好的 sdktrace.TracerProvider 和可能的错误
func InitTracerProvider(version string) (*sdktrace.TracerProvider, error) {
	// 在生产环境中，您会配置一个导出器到真实的追踪后端
	// (例如 Jaeger, Zipkin, 或 OTLP 收集器)。
	// 对于此示例，我们将丢弃追踪数据，以防止它们污染日志。
	exporter, err := stdouttrace.New(stdouttrace.WithWriter(io.Discard))
	if err != nil {
		return nil, err
	}

	// 创建一个资源，用于描述应用程序
	// 包含服务名称和服务版本等属性
	r, err := resource.Merge(
		resource.Default(), // 默认资源，包含一些通用的属性
		resource.NewWithAttributes(
			semconv.SchemaURL,                 // OpenTelemetry 语义约定 Schema URL
			semconv.ServiceName("easy-agent"), // 服务名称
			semconv.ServiceVersion(version),   // 服务版本
		),
	)
	if err != nil {
		return nil, err
	}

	// 创建 TracerProvider
	// WithBatcher 配置了 Span 导出器，这里是丢弃型导出器
	// WithResource 配置了服务资源
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(r),
	)
	// 将创建的 TracerProvider 设置为全局默认
	otel.SetTracerProvider(tp)
	return tp, nil
}
