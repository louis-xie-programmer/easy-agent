
// main 包是程序的入口点，负责初始化服务并与Ollama模型交互
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/louis-xie-programmer/easy-agent/agent"
	"github.com/louis-xie-programmer/easy-agent/web"
)

// main 函数启动HTTP服务器并初始化核心组件
func main() {
	// 从环境变量读取配置参数（OLLAMA_URL/AGENT_ADDR），未设置时使用默认值
  // OLLAMA_URL: 指向Ollama服务的API端点
  // AGENT_ADDR: 代理服务监听地址
  // read config from env or use defaults
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434/api/chat"
	}
	agentEndpoint := os.Getenv("AGENT_ADDR")
	if agentEndpoint == "" {
		agentEndpoint = ":8080"
	}

	// 初始化核心组件
	mem, err := agent.NewFileMemory("agent_memory.json")
	if err != nil {
		log.Fatalf("memory init error: %v", err)
	}
	ollama := agent.NewOllamaClient(ollamaURL, 60*time.Second)
	a := agent.NewAgent(ollama, mem)

	r := mux.NewRouter()
	// RESTful API端点：接收JSON请求并返回AI回答
	// HTTP API: POST /agent { prompt: "..." } -> JSON { answer: "..." }
	r.HandleFunc("/agent", web.AgentHandler(a)).Methods("POST")
	// SSE流式响应端点：支持服务器发送事件
	// SSE streaming: GET /stream?prompt=...
	r.HandleFunc("/stream", web.AgentStreamHandler(a)).Methods("GET")
	// WebSocket API：支持实时双向通信
	r.HandleFunc("/ws", web.WebSocketHandler(a, ollamaURL, "deepseek-r1:1.5b")).Methods("GET")
	// 静态文件服务：提供HTML客户端界面
	r.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir("./client"))))
	// 健康检查端点：返回200表示服务正常
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})

	// 配置HTTP服务器
	srv := &http.Server{
		Handler:      r,
		Addr:         agentEndpoint,
		WriteTimeout: 0,
		ReadTimeout:  30 * time.Second,
	}

	fmt.Printf("Agent listening on %s  (ollama: %s)\n", agentEndpoint, ollamaURL)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
