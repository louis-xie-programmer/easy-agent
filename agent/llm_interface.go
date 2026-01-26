package agent

import (
	"context"
	"io"
)

// LLMProvider 定义了与大语言模型交互的通用接口
// 任何实现了此接口的客户端（Ollama, OpenAI, DeepSeek等）都可以被 Agent 使用
type LLMProvider interface {
	// CallWithContext 发起一次非流式对话
	// ctx: 上下文，用于追踪和取消
	// messages: 对话历史
	// tools: 可用的工具定义（通常是 JSON Schema 数组）
	CallWithContext(ctx context.Context, messages []ChatMessage, tools any) (*ChatResponse, error)

	// StreamCallWithContext 发起一次流式对话
	// ctx: 上下文，用于追踪和取消
	// messages: 对话历史
	// tools: 可用的工具定义
	// writer: 用于写入流式响应的 Writer
	StreamCallWithContext(ctx context.Context, messages []ChatMessage, tools any, writer io.Writer) error

	// Embed 获取文本的向量表示
	// ctx: 上下文，用于追踪
	// text: 输入文本
	// 返回: 浮点数向量
	Embed(ctx context.Context, text string) ([]float64, error)
}
