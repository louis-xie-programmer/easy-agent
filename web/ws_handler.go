package web

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/louis-xie-programmer/easy-agent/agent"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		// allow VS Code / local dev
		return true
	},
}

type WSMessage struct {
	Type    string          `json:"type"`    // "prompt" | "ping"
	Payload json.RawMessage `json:"payload"` // json object depending on type
}

type WSPrompt struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id,omitempty"`
}

// bufferedConnWriter 适配器将WebSocket连接包装为io.Writer接口
// 实现Write方法，将数据作为token消息发送到客户端
// 满足OllamaClient.StreamCall的writer参数要求
type bufferedConnWriter struct {
	conn   *websocket.Conn
	buffer bytes.Buffer
	mu     sync.Mutex
}

func (cw *bufferedConnWriter) Write(p []byte) (n int, err error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	// 将数据累积到缓冲区
	cw.buffer.Write(p)

	// 当缓冲区足够大时才发送
	if cw.buffer.Len() >= 1024 {
		err = cw.flush()
	}

	return len(p), err
}

func (cw *bufferedConnWriter) flush() error {
	if cw.buffer.Len() == 0 {
		return nil
	}

	connMutex.Lock()
	defer connMutex.Unlock()
	
	// 将缓冲区内容作为token消息发送
	err := cw.conn.WriteJSON(map[string]any{
		"type": "token",
		"text": cw.buffer.String(),
	})

	// 重置缓冲区
	cw.buffer.Reset()
	return err
}

// 添加一个映射来跟踪所有活动连接
var (
	clients   = make(map[*websocket.Conn]bool)
	mutex     = sync.RWMutex{}
	connMutex = sync.Mutex{} // 添加连接写入互斥锁
)

// 添加定期ping所有客户端的函数
func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			mutex.RLock()
			clientsCopy := make(map[*websocket.Conn]bool)
			for k, v := range clients {
				clientsCopy[k] = v
			}
			mutex.RUnlock()

			for client := range clientsCopy {
				connMutex.Lock()
				err := client.WriteJSON(map[string]any{
					"type": "ping",
				})
				connMutex.Unlock()
				if err != nil {
					log.Printf("Ping to client failed: %v", err)
					// 移除失效的连接
					mutex.Lock()
					delete(clients, client)
					mutex.Unlock()
				}
			}
		}
	}()
}

func WebSocketHandler(a *agent.Agent, ollamaURL string, model string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("WS upgrade:", err)
			return
		}
		defer conn.Close()

		// 添加连接到客户端列表
		mutex.Lock()
		clients[conn] = true
		mutex.Unlock()

		// 从客户端列表中移除连接
		defer func() {
			mutex.Lock()
			delete(clients, conn)
			mutex.Unlock()
		}()

		log.Println("[WS] client connected")

		// ------------------------------
		// Reader Loop: wait for prompt
		// ------------------------------
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				log.Println("[WS] read error:", err)
				return
			}

			switch msg.Type {

			case "ping":
				connMutex.Lock()
				conn.WriteJSON(map[string]any{"type": "pong"})
				connMutex.Unlock()
				continue

			case "prompt":
				// 解析 prompt 内容
				var p WSPrompt
				if err := json.Unmarshal(msg.Payload, &p); err != nil {
					connMutex.Lock()
					conn.WriteJSON(map[string]any{
						"type":  "error",
						"error": "invalid prompt format",
					})
					connMutex.Unlock()
					continue
				}

				if p.Prompt == "" {
					connMutex.Lock()
					conn.WriteJSON(map[string]any{
						"type":  "error",
						"error": "prompt is empty",
					})
					connMutex.Unlock()
					continue
				}

				// 异步处理（不会阻塞 WS reader）
				go handlePromptWS(conn, a, ollamaURL, model, p.Prompt, p.SessionID)

			default:
				connMutex.Lock()
				conn.WriteJSON(map[string]any{
					"type":  "error",
					"error": "unknown ws event",
				})
				connMutex.Unlock()
			}
		}
	}
}

func handlePromptWS(conn *websocket.Conn, a *agent.Agent, ollamaURL, model, prompt string, sessionID string) {
	// 通知前端开始
	connMutex.Lock()
	err := conn.WriteJSON(map[string]any{
		"type": "status",
		"data": "start_stream",
	})
	connMutex.Unlock()
	if err != nil {
		return
	}

	// 直接使用Agent处理会话，确保会话连续性
	ans, err := a.RunWithSession(prompt, sessionID)
	if err != nil {
		connMutex.Lock()
		conn.WriteJSON(map[string]any{
			"type":  "error",
			"error": err.Error(),
		})
		connMutex.Unlock()
		return
	}

	// 流式发送结果
	for _, char := range ans {
		connMutex.Lock()
		err := conn.WriteJSON(map[string]any{
			"type": "token",
			"text": string(char),
		})
		connMutex.Unlock()
		if err != nil {
			return
		}
		// 简短延迟以模拟流式效果
		time.Sleep(10 * time.Millisecond)
	}

	// 发送完成状态
	connMutex.Lock()
	conn.WriteJSON(map[string]any{
		"type": "done",
		"data": "stream_complete",
	})
	connMutex.Unlock()
}