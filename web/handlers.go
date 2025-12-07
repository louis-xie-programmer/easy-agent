// web 包包含HTTP接口处理逻辑，提供：
// - RESTful API端点
// - SSE流式响应支持
// - 请求解析与响应格式化
// - 会话管理支持
// 所有处理器都包装了核心Agent功能
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/louis-xie-programmer/easy-agent/agent"
)

// AgentRequest 定义API请求结构
type AgentRequest struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id,omitempty"`
}

// AgentResponse 定义API响应结构
type AgentResponse struct {
	Answer    string `json:"answer"`
	SessionID string `json:"session_id"`
}

// SessionCreateRequest 定义创建会话的请求结构
type SessionCreateRequest struct {
	Title string `json:"title"`
}

// SessionCreateResponse 定义创建会话的响应结构
type SessionCreateResponse struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// SessionsListResponse 定义会话列表的响应结构
type SessionsListResponse struct {
	Sessions map[string]map[string]interface{} `json:"sessions"`
}

// AgentHandler 处理POST /agent请求
// 功能：
//   1. 解析JSON请求体
//   2. 调用Agent.RunWithSession执行业务逻辑
//   3. 返回JSON格式的响应
//   4. 处理各种错误情况
// 对应API端点：POST /agent
// POST /agent  body: { "prompt": "...", "session_id": "..." }
// AgentHandler 创建处理函数
// 参数：a 已初始化的Agent实例
// 返回值：符合http.HandlerFunc签名的闭包函数
func AgentHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 解析请求体中的JSON数据
		// 预期格式：{"prompt": "用户输入的提示", "session_id": "可选的会话ID"}
		// 如果解析失败，返回400错误
		var payload AgentRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		
		ans, err := a.RunWithSession(payload.Prompt, payload.SessionID)
		// 处理Agent执行过程中的错误
		// 返回500内部服务器错误
		// 错误信息包含具体的错误描述
		if err != nil {
			http.Error(w, fmt.Sprintf("agent error: %v", err), 500)
			return
		}
		
		response := AgentResponse{
			Answer:    ans,
			SessionID: a.GetMemory().GetCurrentSessionID(),
		}
		
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}
}

// CreateSessionHandler 处理POST /session请求，创建新会话
func CreateSessionHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload SessionCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad request: "+err.Error(), 400)
			return
		}
		
		if payload.Title == "" {
			http.Error(w, "title is required", 400)
			return
		}
		
		// 生成新的会话ID
		sessionID := uuid.New().String()
		
		// 创建会话
		a.GetMemory().CreateSession(sessionID, payload.Title)
		
		response := SessionCreateResponse{
			SessionID: sessionID,
			Message:   fmt.Sprintf("会话 '%s' 已创建", payload.Title),
		}
		
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response)
	}
}

// ListSessionsHandler 处理GET /sessions请求，列出所有会话
func ListSessionsHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessions := a.GetMemory().GetAllSessions()
		response := SessionsListResponse{
			Sessions: sessions,
		}
		
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}
}

// SwitchSessionHandler 处理PUT /session/{id}请求，切换到指定会话
func SwitchSessionHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 从查询参数获取会话ID
		sessionID := r.URL.Query().Get("id")
		if sessionID == "" {
			http.Error(w, "session id is required", 400)
			return
		}
		
		if a.GetMemory().SetCurrentSession(sessionID) {
			response := map[string]string{
				"message": fmt.Sprintf("已切换到会话 ID: %s", sessionID),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		} else {
			http.Error(w, fmt.Sprintf("会话 ID '%s' 不存在", sessionID), 404)
			return
		}
	}
}

// AgentStreamHandler 处理SSE流式请求
// 功能：
//   - 实现服务器发送事件(SSE)
//   - 支持心跳机制保持连接
//   - 异步执行代理任务
//   - 连接关闭检测
// 对应API端点：GET /stream
// GET /stream?prompt=...&session_id=...
// AgentStreamHandler 创建SSE处理函数
// 参数：a 已初始化的Agent实例
// 返回值：符合http.HandlerFunc签名的闭包函数
func AgentStreamHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("prompt")
		sessionID := r.URL.Query().Get("session_id")
		
		if p == "" {
			http.Error(w, "prompt required", 400)
			return
		}
		
		// Basic SSE streaming: send simple events (not full chunked streaming with intermediate model events)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// 使用ticker定时发送心跳事件
		// 保持长连接活跃状态
		// 心跳间隔：5秒
		// For demo: run agent.Run but emit heartbeat and final answer
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		notify := w.(http.CloseNotifier).CloseNotify()
		// done channel用于通知主goroutine停止
		done := make(chan struct{})
		// 启动一个goroutine来监听客户端连接关闭事件
		// 当检测到连接断开时，通过done channel通知主循环
		go func() {
			select {
			case <-notify:
				close(done)
			}
		}()

		// 初始化JSON编码器和刷新器
		enc := json.NewEncoder(w)
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		
		// 发送初始的meta事件
		// 表示流式响应已开始
		// heartbeat
		fmt.Fprintf(w, "event: meta\ndata: %s\n\n", `{"status":"started"}`)
		flusher.Flush()

		// 启动一个goroutine异步执行代理任务
		// 这样可以避免阻塞HTTP响应流
		// 执行完成后将结果编码为JSON并通过SSE发送
		// 最后关闭done channel通知主循环结束
		go func() {
			// 检查连接是否已关闭，避免向已关闭的连接写入
			select {
			case <-done:
				return
			default:
			}

			ans, err := a.RunWithSession(p, sessionID)
			var out map[string]string
			if err != nil {
				out = map[string]string{"error": err.Error()}
			} else {
				out = map[string]string{"answer": ans}
			}

			// 安全写入并处理可能的连接错误
			if err := enc.Encode(out); err != nil {
				return // 客户端已断开连接
			}
			fmt.Fprint(w, "\n\n")
			flusher.Flush()
			close(done)
		}()

		// 主循环：持续监听两个事件源
		// 1. 客户端连接关闭（<-done）
		// 2. 心跳定时器（<-ticker.C）
		// keep connection until done
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(w, "event: heartbeat\ndata: %s\n\n", `{"time": "`+time.Now().Format(time.RFC3339)+`"}`)
				flusher.Flush()
			}
		}
	}
}