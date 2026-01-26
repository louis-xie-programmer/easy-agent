// main 包是程序的入口点，负责初始化服务并与Ollama模型交互
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/louis-xie-programmer/easy-agent/agent"
	"github.com/louis-xie-programmer/easy-agent/web"
)

// main 函数启动HTTP服务器并初始化核心组件
func main() {
	// 加载应用程序配置
	cfg, err := agent.LoadConfig()
	if err != nil {
		// 如果日志系统尚未初始化，则使用标准错误输出记录致命错误
		os.Stderr.WriteString("FATAL: failed to load config: " + err.Error())
		os.Exit(1)
	}

	// 初始化异步日志系统
	agent.InitLogger(cfg)
	// 使用 defer 确保日志系统在 main 函数退出时被关闭，释放资源
	defer agent.CloseLogger()

	// 初始化 OpenTelemetry Tracer Provider，用于分布式追踪
	tp, err := agent.InitTracerProvider(cfg.Service.Version)
	if err != nil {
		agent.Logger.Fatal().Err(err).Msg("Failed to init tracer provider")
	}
	// 使用 defer 确保 Tracer Provider 在 main 函数退出时被关闭，刷新所有待处理的 Span
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			agent.Logger.Error().Err(err).Msg("Error shutting down tracer provider")
		}
	}()

	// 初始化会话记忆存储
	mem, err := agent.NewMemoryV3(cfg.Storage.MemoryPath)
	if err != nil {
		agent.Logger.Fatal().Err(err).Msg("Memory init error")
	}
	// 使用 defer 确保会话记忆存储在 main 函数退出时被关闭，保存数据
	defer func() {
		if err := mem.Close(); err != nil {
			agent.Logger.Error().Err(err).Msg("Error closing memory")
		}
	}()

	// 初始化向量存储，用于 RAG (检索增强生成)
	vectorStore, err := agent.NewInMemoryVectorStore(cfg.Storage.VectorPath)
	if err != nil {
		agent.Logger.Fatal().Err(err).Msg("Vector store init error")
	}
	// 使用 defer 确保向量存储在 main 函数退出时被关闭，保存数据
	defer func() {
		if err := vectorStore.Close(); err != nil {
			agent.Logger.Error().Err(err).Msg("Error closing vector store")
		}
	}()

	// 创建 Ollama 客户端，用于与大语言模型交互
	ollama := agent.NewOllamaClient(cfg)
	// 创建 Agent 核心实例，协调 LLM、记忆、工具和向量存储
	a := agent.NewAgent(ollama, mem, vectorStore, cfg)

	// 创建一个新的 HTTP 路由器
	r := mux.NewRouter()
	// 注册所有 HTTP 路由和处理器
	web.RegisterRoutes(r, a, cfg)

	// 配置 CORS (跨域资源共享) 中间件
	// 允许所有来源、所有常用 HTTP 方法和指定头部，在生产环境中应根据实际需求限制 AllowedOrigins
	corsHandler := handlers.CORS(
		handlers.AllowedOrigins([]string{"*"}), // 允许所有来源，开发环境方便，生产环境建议指定具体域名
		handlers.AllowedMethods([]string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}),
		handlers.AllowedHeaders([]string{"Content-Type", "X-Requested-With"}),
	)

	// 配置 HTTP 服务器
	srv := &http.Server{
		Handler:      corsHandler(r), // 将 CORS 中间件应用于路由器
		Addr:         cfg.Server.Address,
		WriteTimeout: 0, // 对于流式响应，写入超时设置为 0 (无超时)
		ReadTimeout:  30 * time.Second,
	}

	// 在一个独立的 goroutine 中启动 HTTP 服务器，避免阻塞主线程
	go func() {
		agent.Logger.Info().
			Str("address", cfg.Server.Address).
			Str("ollama_url", cfg.Ollama.URL).
			Str("default_model", cfg.Ollama.DefaultModel).
			Msg("Agent listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			agent.Logger.Fatal().Err(err).Msg("Server error")
		}
	}()

	// 等待操作系统中断信号 (SIGINT, SIGTERM) 以实现优雅停机
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // 阻塞直到接收到信号
	agent.Logger.Info().Msg("Shutting down server...")

	// 创建一个带有超时的上下文，用于通知服务器它有30秒的时间来完成当前正在处理的请求
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel() // 确保上下文在操作完成后被取消

	// 优雅地关闭 HTTP 服务器
	if err := srv.Shutdown(ctx); err != nil {
		agent.Logger.Fatal().Err(err).Msg("Server forced to shutdown")
	}

	agent.Logger.Info().Msg("Server exiting")
}
