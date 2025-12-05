package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
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
	Prompt string `json:"prompt"`
}

func WebSocketHandler(a *agent.Agent, ollamaURL string, model string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("WS upgrade:", err)
			return
		}
		defer conn.Close()

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
				conn.WriteJSON(map[string]any{"type": "pong"})
				continue

			case "prompt":
				// 解析 prompt 内容
				var p WSPrompt
				json.Unmarshal(msg.Payload, &p)

				if p.Prompt == "" {
					conn.WriteJSON(map[string]any{
						"type":  "error",
						"error": "prompt is empty",
					})
					continue
				}

				// 异步处理（不会阻塞 WS reader）
				go handlePromptWS(conn, a, ollamaURL, model, p.Prompt)

			default:
				conn.WriteJSON(map[string]any{
					"type":  "error",
					"error": "unknown ws event",
				})
			}
		}
	}
}

func handlePromptWS(conn *websocket.Conn, a *agent.Agent, ollamaURL, model, prompt string) {

	// 通知前端开始
	conn.WriteJSON(map[string]any{
		"type": "status",
		"data": "start_stream",
	})

	// 构造请求给 Ollama
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是一个本地 DeepSeek-R1 智能体，资深的 go 编程专家。"},
			{"role": "user", "content": prompt},
		},
		"stream": true,
	}

	bs, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", ollamaURL, bytes.NewReader(bs))
	if err != nil {
		conn.WriteJSON(map[string]any{"type": "error", "error": err.Error()})
		return
	}

	req.Header.Set("Content-Type", "application/json")

	// 使用与OllamaClient相同的超时时间
	ctx, cancel := context.WithTimeout(context.Background(), 3000*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		conn.WriteJSON(map[string]any{
			"type":  "warn",
			"error": "ollama no stream; fallback to agent",
		})

		// fallback：一次性结果
		ans, _ := a.Run(prompt)

		conn.WriteJSON(map[string]any{
			"type": "final",
			"text": ans,
		})
		return
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	buf := make([]byte, 0, 4096)

	// 流式 token 读取
	for {
		line, isPrefix, err := reader.ReadLine()

		if err != nil {
			// 区分不同类型的错误
			netErr, ok := err.(net.Error)
			if ok && netErr.Timeout() {
				log.Printf("Ollama响应超时: %v", err)
			} else {
				log.Printf("流式读取错误: %v", err)
			}
			break
		}

		buf = append(buf, line...)
		if isPrefix {
			continue
		}

		chunk := string(buf)
		buf = buf[:0]

		// 解析Ollama流式响应格式
		var response struct {
			Model     string `json:"model"`
			CreatedAt string `json:"created_at"`
			Message   struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			Thinking string `json:"thinking"`
			Done     bool   `json:"done"`
		}

		if err := json.Unmarshal([]byte(chunk), &response); err == nil {
			// 优先处理thinking字段（如果有）
			if response.Thinking != "" {
				conn.WriteJSON(map[string]any{
					"type": "token",
					"text": response.Thinking,
				})
				// 处理content字段
			} else if response.Message.Content != "" {
				conn.WriteJSON(map[string]any{
					"type": "token",
					"text": response.Message.Content,
				})
			}
			// 处理完成状态
			if response.Done {
				conn.WriteJSON(map[string]any{
					"type": "done",
					"data": "stream_complete",
				})
				return // 正常结束，返回避免重复发送done
			}
		} else {
			log.Printf("无法解析Ollama响应: %s", chunk)
		}
	}

	// 确保结束事件只发送一次
	select {
	case <-ctx.Done():
		log.Println("请求上下文已取消")
	default:
		// 只有在非正常结束时才发送done
		conn.WriteJSON(map[string]any{
			"type": "done",
			"data": "stream_complete",
		})
	}

}
