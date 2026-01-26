package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/louis-xie-programmer/easy-agent/agent"

	"github.com/gorilla/websocket"
)

// upgrader 用于将 HTTP 连接升级为 WebSocket 连接
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096, // 读取缓冲区大小
	WriteBufferSize: 4096, // 写入缓冲区大小
	CheckOrigin: func(r *http.Request) bool {
		// 允许所有来源的 WebSocket 连接，方便 VS Code / 本地开发
		// 在生产环境中，应根据安全策略限制允许的来源
		return true
	},
}

// WSMessage 定义了 WebSocket 通信中通用消息的结构
type WSMessage struct {
	Type    string          `json:"type"`    // 消息类型，例如 "prompt" | "ping" | "tool_confirmation" | "stop"
	Payload json.RawMessage `json:"payload"` // 消息负载，JSON 对象，具体内容取决于消息类型
}

// WSPrompt 定义了 "prompt" 类型消息的负载结构
type WSPrompt struct {
	Prompt    string   `json:"prompt"`               // 用户输入的提示词
	SessionID string   `json:"session_id,omitempty"` // 会话 ID，可选
	Images    []string `json:"images,omitempty"`     // Base64 编码的图片数据，支持多模态
	Model     string   `json:"model,omitempty"`      // 指定使用的模型名称，可选
}

// WSConfirmation 定义了 "tool_confirmation" 类型消息的负载结构
type WSConfirmation struct {
	ConfirmationID string `json:"confirmation_id"` // 确认请求的 ID
	Allowed        bool   `json:"allowed"`         // 用户是否允许执行操作 (true 表示允许，false 表示拒绝)
}

// Client 是 WebSocket 连接的封装，包含一个互斥锁以确保对连接的写入是线程安全的。
type Client struct {
	conn       *websocket.Conn    // WebSocket 连接实例
	mu         sync.Mutex         // 互斥锁，用于保护对 conn 的写入操作
	cancelFunc context.CancelFunc // 用于取消当前操作的函数
	cancelMu   sync.Mutex         // 互斥锁，用于保护 cancelFunc 的并发访问
}

// SafeWriteJSON 安全地将 JSON 消息写入 WebSocket 连接。
func (c *Client) SafeWriteJSON(v interface{}) error {
	c.mu.Lock() // 获取写入锁
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v) // 写入 JSON 消息
}

// SetCancelFunc 设置当前操作的取消函数。
func (c *Client) SetCancelFunc(cancel context.CancelFunc) {
	c.cancelMu.Lock() // 获取锁
	defer c.cancelMu.Unlock()
	c.cancelFunc = cancel // 设置取消函数
}

// Cancel 调用取消函数（如果存在），以取消当前正在进行的操作。
func (c *Client) Cancel() {
	c.cancelMu.Lock() // 获取锁
	defer c.cancelMu.Unlock()
	if c.cancelFunc != nil {
		c.cancelFunc()     // 调用取消函数
		c.cancelFunc = nil // 清空取消函数
	}
}

// clients 映射用于跟踪所有活跃的客户端连接。
var (
	clients = make(map[*Client]bool) // 客户端映射
	// clientsMutex 是一个互斥锁，用于保护 clients 映射本身的并发访问
	clientsMutex = sync.RWMutex{}
)

// init 函数在包加载时执行，用于启动一个 goroutine，定期向所有客户端发送 ping 消息，
// 以保持连接活跃并清理已断开的连接。
func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Second) // 每 30 秒发送一次 ping
		defer ticker.Stop()

		for range ticker.C {
			// 创建客户端列表的副本以进行迭代，避免在迭代时持有锁
			clientsMutex.RLock()
			clientsCopy := make([]*Client, 0, len(clients))
			for c := range clients {
				clientsCopy = append(clientsCopy, c)
			}
			clientsMutex.RUnlock()

			for _, client := range clientsCopy {
				err := client.SafeWriteJSON(map[string]any{
					"type": "ping", // 发送 ping 消息
				})
				if err != nil {
					log.Printf("Ping to client failed, removing: %v", err)
					// 移除已断开的连接
					clientsMutex.Lock()
					delete(clients, client)
					clientsMutex.Unlock()
					client.conn.Close() // 确保连接已关闭
				}
			}
		}
	}()
}

// WebSocketHandler 处理 WebSocket 连接请求
// a: Agent 核心实例
func WebSocketHandler(a *agent.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		// 将 HTTP 连接升级为 WebSocket 连接
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("WS upgrade:", err)
			return
		}
		defer conn.Close() // 确保 WebSocket 连接在函数退出时关闭

		client := &Client{conn: conn} // 创建新的客户端实例

		// 将新客户端添加到活跃客户端列表中
		clientsMutex.Lock()
		clients[client] = true
		clientsMutex.Unlock()

		// 确保客户端在处理程序退出时从列表中移除
		defer func() {
			clientsMutex.Lock()
			delete(clients, client)
			clientsMutex.Unlock()
			log.Println("[WS] client disconnected")
		}()

		log.Println("[WS] client connected")

		// ------------------------------
		// 读取循环：等待来自客户端的消息
		// ------------------------------
		for {
			var msg WSMessage
			// 读取 JSON 格式的 WebSocket 消息
			if err := conn.ReadJSON(&msg); err != nil {
				// 检查是否为意外的关闭错误
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Println("[WS] read error:", err)
				}
				return // 退出循环，关闭连接
			}

			switch msg.Type {

			case "ping":
				client.SafeWriteJSON(map[string]any{"type": "pong"}) // 回复 pong 消息
				continue

			case "stop":
				client.Cancel() // 取消当前正在进行的 Agent 操作
				client.SafeWriteJSON(agent.StreamEvent{
					Type:    "status",
					Payload: map[string]string{"status": "stopped_by_user"}, // 通知客户端操作已停止
				})
				continue

			case "prompt":
				var p WSPrompt
				// 解析提示消息负载
				if err := json.Unmarshal(msg.Payload, &p); err != nil {
					client.SafeWriteJSON(agent.StreamEvent{
						Type:    "error",
						Payload: agent.ErrorEventPayload{Message: "invalid prompt format"},
					})
					continue
				}

				// 验证提示或图片是否为空
				if p.Prompt == "" && len(p.Images) == 0 {
					client.SafeWriteJSON(agent.StreamEvent{
						Type:    "error",
						Payload: agent.ErrorEventPayload{Message: "prompt or image is required"},
					})
					continue
				}

				// 在新的 goroutine 中处理提示，避免阻塞读取循环
				go handlePromptWS(client, a, r.Context(), p)

			case "tool_confirmation":
				var c WSConfirmation
				// 解析工具确认消息负载
				if err := json.Unmarshal(msg.Payload, &c); err != nil {
					client.SafeWriteJSON(agent.StreamEvent{
						Type:    "error",
						Payload: agent.ErrorEventPayload{Message: "invalid confirmation format"},
					})
					continue
				}
				// 解决工具确认请求
				a.GetConfirmationManager().ResolveRequest(c.ConfirmationID, c.Allowed)

			default:
				client.SafeWriteJSON(agent.StreamEvent{
					Type:    "error",
					Payload: agent.ErrorEventPayload{Message: "unknown ws event type"},
				})
			}
		}
	}
}

// handlePromptWS 在独立的 goroutine 中处理 WebSocket 提示消息
// client: WebSocket 客户端实例
// a: Agent 核心实例
// parentCtx: 父上下文
// p: 提示消息负载
func handlePromptWS(client *Client, a *agent.Agent, parentCtx context.Context, p WSPrompt) {
	// 为此特定请求创建一个可取消的上下文
	ctx, cancel := context.WithCancel(parentCtx)
	client.SetCancelFunc(cancel)    // 设置取消函数
	defer client.SetCancelFunc(nil) // 在退出时清理取消函数

	// 通知前端流式响应即将开始
	client.SafeWriteJSON(agent.StreamEvent{
		Type:    "status",
		Payload: map[string]string{"status": "start_stream"},
	})

	// 创建一个通道以接收来自 Agent 的流式事件
	events := make(chan agent.StreamEvent)

	// 在新的 goroutine 中启动 Agent 的流式处理
	// 传入可取消的上下文
	go a.StreamRunWithSessionAndImages(ctx, p.Prompt, p.SessionID, p.Images, p.Model, events)

	// 将来自 Agent 的事件转发到 WebSocket 客户端
	for event := range events {
		if err := client.SafeWriteJSON(event); err != nil {
			log.Printf("Write to websocket error: %v", err)
			// 如果客户端已断开连接，则停止转发
			break
		}
	}

	// 通知前端流式响应已完成
	client.SafeWriteJSON(agent.StreamEvent{
		Type:    "status",
		Payload: map[string]string{"status": "stream_complete"},
	})
}
