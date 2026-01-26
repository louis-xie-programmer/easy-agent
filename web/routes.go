package web

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/louis-xie-programmer/easy-agent/agent"
)

// RegisterRoutes 注册应用程序的所有 HTTP 路由和处理器
// r: Gorilla Mux 路由器实例
// a: Agent 核心实例，用于处理业务逻辑
// cfg: 应用程序配置
func RegisterRoutes(r *mux.Router, a *agent.Agent, cfg agent.Config) {
	// RESTful API 端点：接收 JSON 请求并返回 AI 回答
	// HTTP API: POST /agent { prompt: "..." } -> JSON { answer: "..." }
	r.HandleFunc("/agent", AgentHandler(a)).Methods("POST")

	// 会话管理端点
	r.HandleFunc("/session", CreateSessionHandler(a)).Methods("POST")                   // 创建新会话
	r.HandleFunc("/session", SwitchSessionHandler(a)).Methods("PUT")                    // 切换会话
	r.HandleFunc("/sessions", ListSessionsHandler(a)).Methods("GET")                    // 列出所有会话
	r.HandleFunc("/session/{id}/messages", GetSessionMessagesHandler(a)).Methods("GET") // 获取指定会话的消息历史

	// 配置端点
	r.HandleFunc("/config/models", GetModelsHandler(cfg)).Methods("GET") // 获取可用模型列表

	// 文件上传端点 (RAG - 检索增强生成)
	r.HandleFunc("/upload", UploadHandler(a)).Methods("POST") // 上传文件并入库

	// SSE 流式响应端点：支持服务器发送事件
	// SSE streaming: GET /stream?prompt=...
	r.HandleFunc("/stream", AgentStreamHandler(a)).Methods("GET") // 流式获取 AI 响应

	// WebSocket API：支持实时双向通信
	r.HandleFunc("/ws", WebSocketHandler(a)).Methods("GET") // WebSocket 连接端点

	// 静态文件服务：提供 HTML 客户端界面
	// 将所有未匹配的路径请求映射到静态文件目录
	r.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir(cfg.Server.StaticPath))))

	// 健康检查端点：返回 200 表示服务正常运行
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
}
