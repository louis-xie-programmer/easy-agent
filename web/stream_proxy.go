package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/louis-xie-programmer/easy-agent/agent"
)

// AgentStreamProxyHandler:
// - 尝试向 Ollama 发出流式请求（如果模型/服务支持 chunked/streaming）
// - 将收到的每个 chunk 逐条封装为 SSE data 并发送给客户端
// - 如果 Ollama 未提供流式响应，回退到 agent.Run 并发送最终 answer
//
// Notes:
// - This code proxies model chunks raw. To do structured events (tool calls, partial tokens),
//   parse JSON frames from the chunk stream and emit specific SSE event types.
func AgentStreamProxyHandler(a *agent.Agent, ollamaURL string, model string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Expect prompt in query or body
		prompt := r.URL.Query().Get("prompt")
		if prompt == "" {
			var body struct {
				Prompt string `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			prompt = body.Prompt
		}
		if prompt == "" {
			http.Error(w, "prompt required", 400)
			return
		}

		// Prepare SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}

		// Heartbeat
		fmt.Fprintf(w, "event: meta\ndata: %s\n\n", `{"status":"stream_start"}`)
		flusher.Flush()

		// Try to request Ollama with streaming preference.
		// We won't assume Ollama requires "stream": true, but include it as optional convenience.
		type chatMsg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		reqBody := map[string]any{
			"model": model,
			"messages": []chatMsg{
				{Role: "system", Content: "你是一个模型，会在需要时用 function_call 请求工具。"},
				{Role: "user", Content: prompt},
			},
			// optional: some LLM endpoints accept "stream": true. If Ollama supports it, it may stream.
			"stream": true,
		}
		bs, _ := json.Marshal(reqBody)
		ollamaReq, err := http.NewRequest("POST", ollamaURL, bytes.NewReader(bs))
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonEscape(map[string]string{"error": err.Error()}))
			flusher.Flush()
			return
		}
		ollamaReq.Header.Set("Content-Type", "application/json")
		// If your Ollama needs auth, set header here (optional)
		// ollamaReq.Header.Set("Authorization", "Bearer "+os.Getenv("OLLAMA_API_KEY"))

		// 使用更长的超时时间以适应大模型响应
		ctx, cancel := context.WithTimeout(r.Context(), 3000*time.Second)
		defer cancel()
		ollamaReq = ollamaReq.WithContext(ctx)

		client := &http.Client{}
		resp, err := client.Do(ollamaReq)
		if err != nil {
			// fallback: run synchronous agent.Run and stream final answer
			fmt.Fprintf(w, "event: warn\ndata: %s\n\n", jsonEscape(map[string]string{"warn": "ollama call failed, falling back to sync agent", "err": err.Error()}))
			flusher.Flush()
			streamFallbackRun(a, prompt, w, flusher)
			return
		}
		defer resp.Body.Close()

		// If server responded with non-200 or not chunked, attempt to stream raw body, else fallback.
		if resp.StatusCode >= 400 {
			bodyText, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonEscape(map[string]string{"error": fmt.Sprintf("ollama status %d", resp.StatusCode), "body": string(bodyText)}))
			flusher.Flush()
			// fallback to agent.Run
			streamFallbackRun(a, prompt, w, flusher)
			return
		}

		// If Content-Length present and small, treat as non-streaming; but still try streaming read.
		// We'll read from resp.Body as a stream and forward chunks as SSE `data:` lines.
		reader := bufio.NewReader(resp.Body)
		buf := make([]byte, 0, 4096)
		for {
			// read a line/chunk (non-blocking read until newline)
			line, isPrefix, err := reader.ReadLine()
			if err != nil {
				if err == io.EOF {
					// finished streaming
					break
				}
				// on error, log to client and break
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonEscape(map[string]string{"error": err.Error()}))
				flusher.Flush()
				break
			}
			// accumulate chunk
			buf = append(buf, line...)
			if isPrefix {
				// line too long, continue reading
				continue
			}

			// one line chunk finished -> forward as SSE
			chunk := string(buf)
			// Some chunk protocols send JSON frames like: {"delta":"..."} or plain text.
			// Here we forward raw chunk as data event. Frontend parses/concats.
			fmt.Fprintf(w, "data: %s\n\n", sseEscape(chunk))
			flusher.Flush()

			// reset buffer
			buf = buf[:0]
		}

		// final flush and finish
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", jsonEscape(map[string]string{"status": "complete"}))
		flusher.Flush()
	}
}

// streamFallbackRun runs the synchronous Agent.Run and sends one SSE data with the final answer.
func streamFallbackRun(a *agent.Agent, prompt string, w http.ResponseWriter, flusher http.Flusher) {
	ans, err := a.Run(prompt)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonEscape(map[string]string{"error": err.Error()}))
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", jsonEscape(map[string]string{"answer": ans}))
	flusher.Flush()
}

// sseEscape ensures the data line does not contain characters that break SSE framing
func sseEscape(s string) string {
	// SSE data lines must not contain \r\n; replace them with \n and escape leading "data: " sequences if needed.
	// We also escape newlines by splitting into multiple data: lines is valid, but here we replace CR and keep \n.
	replaced := bytes.ReplaceAll([]byte(s), []byte("\r"), []byte(""))
	replaced = bytes.ReplaceAll(replaced, []byte("\n"), []byte("\\n"))
	// simple JSON style quoting to be safe
	escaped, _ := json.Marshal(string(replaced))
	// json.Marshal returns quoted string, remove surrounding quotes
	if len(escaped) >= 2 {
		return string(escaped[1 : len(escaped)-1])
	}
	return string(escaped)
}

func jsonEscape(m any) string {
	b, _ := json.Marshal(m)
	// return JSON string encoded, but SSE data must be a single line; replace newlines
	s := bytes.ReplaceAll(b, []byte("\n"), []byte("\\n"))
	return string(s)
}
