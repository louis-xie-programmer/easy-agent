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
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/louis-xie-programmer/easy-agent/agent"
)

// allowedExtensions 定义了允许上传的文件扩展名白名单
var allowedExtensions = map[string]bool{
	".txt": true,
	".md":  true,
	".pdf": true,
}

// AgentRequest 定义了 /agent 接口的请求结构
type AgentRequest struct {
	Prompt    string `json:"prompt"`               // 用户输入的提示词
	SessionID string `json:"session_id,omitempty"` // 会话 ID，可选
	Model     string `json:"model,omitempty"`      // 指定使用的模型，可选
}

// AgentResponse 定义了 /agent 接口的响应结构
type AgentResponse struct {
	Answer    string `json:"answer"`     // AI 的回答内容
	SessionID string `json:"session_id"` // 当前会话 ID
}

// SessionCreateRequest 定义了创建会话接口的请求结构
type SessionCreateRequest struct {
	Title string `json:"title"` // 会话标题
}

// SessionCreateResponse 定义了创建会话接口的响应结构
type SessionCreateResponse struct {
	SessionID string `json:"session_id"` // 新创建的会话 ID
	Message   string `json:"message"`    // 成功消息
}

// SessionsListResponse 定义了获取会话列表接口的响应结构
type SessionsListResponse struct {
	Sessions map[string]map[string]interface{} `json:"sessions"` // 会话列表映射
}

// SessionMessagesResponse 定义了获取会话消息接口的响应结构
type SessionMessagesResponse struct {
	Messages []agent.ChatMessage `json:"messages"` // 会话中的消息列表
}

// ModelsResponse 定义了获取模型列表接口的响应结构
type ModelsResponse struct {
	Models []string `json:"models"` // 可用模型名称列表
}

// AgentHandler 处理 POST /agent 请求 (非流式)
// 接收用户提示，调用 Agent 进行处理，并返回完整的 JSON 响应
func AgentHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload AgentRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad request", 400)
			return
		}

		// 使用流式方法，但在内部聚合结果，以便复用 Agent 的核心逻辑
		events := make(chan agent.StreamEvent)
		go a.StreamRunWithSessionAndImages(r.Context(), payload.Prompt, payload.SessionID, nil, payload.Model, events)

		var finalAnswer strings.Builder
		var toolOutput strings.Builder
		var lastError string

		// 消费事件流并聚合结果
		for event := range events {
			switch event.Type {
			case "token":
				if p, ok := event.Payload.(agent.TokenEventPayload); ok {
					finalAnswer.WriteString(p.Text)
				}
			case "tool_output":
				if p, ok := event.Payload.(agent.ToolOutputEventPayload); ok {
					toolOutput.WriteString(p.Output)
				}
			case "final_answer":
				if p, ok := event.Payload.(agent.FinalAnswerEventPayload); ok {
					finalAnswer.WriteString(p.Text)
				}
			case "error":
				if p, ok := event.Payload.(agent.ErrorEventPayload); ok {
					lastError = p.Message
				}
			}
		}

		if lastError != "" {
			http.Error(w, fmt.Sprintf("agent error: %v", lastError), 500)
			return
		}

		// 如果有工具输出但没有最终答案，将工具输出作为答案返回
		answer := finalAnswer.String()
		if answer == "" && toolOutput.Len() > 0 {
			answer = toolOutput.String()
		}

		response := AgentResponse{
			Answer:    answer,
			SessionID: a.GetMemory().GetCurrentSessionID(),
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			agent.Logger.Error().Err(err).Msg("Failed to encode agent response")
		}
	}
}

// CreateSessionHandler 处理 POST /session 请求，创建新会话
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
		if err := json.NewEncoder(w).Encode(response); err != nil {
			agent.Logger.Error().Err(err).Msg("Failed to encode session creation response")
		}
	}
}

// ListSessionsHandler 处理 GET /sessions 请求，列出所有会话
func ListSessionsHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessions := a.GetMemory().GetAllSessions()
		response := SessionsListResponse{
			Sessions: sessions,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			agent.Logger.Error().Err(err).Msg("Failed to encode session list response")
		}
	}
}

// GetSessionMessagesHandler 处理 GET /session/{id}/messages 请求，获取指定会话的历史消息
func GetSessionMessagesHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		sessionID := vars["id"]
		if sessionID == "" {
			http.Error(w, "session id is required", 400)
			return
		}

		msgs, exists := a.GetMemory().GetSessionMessages(sessionID)
		if !exists {
			http.Error(w, "session not found", 404)
			return
		}

		response := SessionMessagesResponse{
			Messages: msgs,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			agent.Logger.Error().Err(err).Msg("Failed to encode session messages response")
		}
	}
}

// GetModelsHandler 处理 GET /config/models 请求，获取可用模型列表
func GetModelsHandler(cfg agent.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := ModelsResponse{
			Models: cfg.Ollama.Models,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			agent.Logger.Error().Err(err).Msg("Failed to encode models response")
		}
	}
}

// SwitchSessionHandler 处理 PUT /session/{id} 请求，切换到指定会话
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
			if err := json.NewEncoder(w).Encode(response); err != nil {
				agent.Logger.Error().Err(err).Msg("Failed to encode switch session response")
			}
		} else {
			http.Error(w, fmt.Sprintf("会话 ID '%s' 不存在", sessionID), 404)
			return
		}
	}
}

// UploadHandler 处理文件上传请求，并将文件内容入库到向量存储 (RAG)
func UploadHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 限制上传大小为 10MB
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "file too large", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "invalid file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// 清理文件名以防止路径遍历攻击
		filename := filepath.Base(header.Filename)

		// 验证文件扩展名是否在白名单中
		ext := filepath.Ext(filename)
		if !allowedExtensions[ext] {
			http.Error(w, fmt.Sprintf("file type %s not allowed", ext), http.StatusBadRequest)
			return
		}

		// 读取文件内容
		contentBytes, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "read file error", http.StatusInternalServerError)
			return
		}
		content := string(contentBytes)

		// 异步处理入库，避免阻塞 HTTP 响应
		go func() {
			if err := a.IngestContent(filename, content); err != nil {
				agent.Logger.Error().Err(err).Str("filename", filename).Msg("Ingest failed")
			}
		}()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"message": fmt.Sprintf("文件 '%s' 已接收，正在后台处理...", filename),
		}); err != nil {
			agent.Logger.Error().Err(err).Msg("Failed to encode upload response")
		}
	}
}

// AgentStreamHandler 处理 SSE (Server-Sent Events) 流式请求
// 允许客户端实时接收 AI 的思考过程、工具调用和最终回答
func AgentStreamHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("prompt")
		sessionID := r.URL.Query().Get("session_id")
		model := r.URL.Query().Get("model")

		if p == "" {
			http.Error(w, "prompt required", 400)
			return
		}

		// 设置 SSE 相关的 HTTP 头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}

		events := make(chan agent.StreamEvent)
		// 启动 Agent 的流式处理
		go a.StreamRunWithSessionAndImages(r.Context(), p, sessionID, nil, model, events)

		// 将事件实时推送到客户端
		for event := range events {
			jsonBytes, err := json.Marshal(event)
			if err != nil {
				log.Printf("Error marshaling stream event: %v", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonBytes)
			flusher.Flush()
		}
	}
}
