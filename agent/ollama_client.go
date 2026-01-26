// ollama.go
// agent 包中的Ollama客户端模块，负责：
// - 定义与Ollama兼容的API请求/响应结构体
// - 实现HTTP通信逻辑
// - 处理认证和错误情况
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Ollama-compatible request/response minimal types

// ToolCallFunction 定义工具调用的具体函数信息
type ToolCallFunction struct {
	Name      string                 `json:"name"`      // 函数名称
	Arguments map[string]interface{} `json:"arguments"` // 函数参数，JSON 对象格式
}

// ToolCall 表示模型建议执行的工具调用
// 对应 Ollama/OpenAI API 的 tool_calls 列表项
type ToolCall struct {
	Type     string           `json:"type"`     // 工具类型，通常为 "function"
	Function ToolCallFunction `json:"function"` // 工具函数的具体信息
}

// ChatMessage 表示对话中的一条消息
// 符合OpenAI API的消息格式规范
type ChatMessage struct {
	Role      string     `json:"role"`                 // 角色（system/user/assistant/tool）
	Content   string     `json:"content,omitempty"`    // 消息内容文本
	Name      string     `json:"name,omitempty"`       // 工具调用时的函数名称
	Images    []string   `json:"images,omitempty"`     // 图片数据（Base64编码），支持多模态
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // 助手消息中的工具调用列表
}

// ChatRequest 封装发送给Ollama模型的完整请求
type ChatRequest struct {
	Model      string        `json:"model"`                 // 使用的模型名称
	Messages   []ChatMessage `json:"messages"`              // 对话历史消息数组
	Tools      any           `json:"tools,omitempty"`       // 可用工具的元数据描述
	ToolChoice string        `json:"tool_choice,omitempty"` // 工具选择策略（auto/manual/none）
	Stream     bool          `json:"stream,omitempty"`      // 是否启用流式响应
}

// FunctionCall 表示模型建议执行的函数调用 (Legacy 兼容)
type FunctionCall struct {
	Name      string          `json:"name"`      // 函数名称
	Arguments json.RawMessage `json:"arguments"` // JSON格式的参数字符串
}

// ChoiceMessage 表示模型返回的选择结果中的消息部分
// 包含角色、内容以及可能的函数调用指令
type ChoiceMessage struct {
	Role         string        `json:"role"`                    // 角色
	Content      string        `json:"content,omitempty"`       // 消息内容
	FunctionCall *FunctionCall `json:"function_call,omitempty"` // 兼容旧版 FunctionCall
	Name         string        `json:"name,omitempty"`          // 工具名称
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`    // 工具调用列表
}

// Choice 表示一个完整的响应选项
// Ollama API可能返回多个选择，当前只处理第一个
type Choice struct {
	Message      ChoiceMessage `json:"message"`                 // 消息内容
	FinishReason string        `json:"finish_reason,omitempty"` // 结束原因
}

// ChatResponse 表示从Ollama接收到的完整响应
type ChatResponse struct {
	Choices []Choice `json:"choices"` // 响应选项数组（通常只有一个）
	// Ollama 可能包含其他字段
}

// OllamaClient 封装与Ollama服务的通信
type OllamaClient struct {
	url    string       // Ollama API 端点 URL
	client *http.Client // HTTP 客户端实例
	model  string       // 默认使用的模型名称
	cfg    Config       // 应用程序配置
}

// 确保 OllamaClient 实现了 LLMProvider 接口
var _ LLMProvider = (*OllamaClient)(nil)

// NewOllamaClient 创建新的Ollama客户端实例
// cfg: 应用程序配置
func NewOllamaClient(cfg Config) *OllamaClient {
	// 从配置中获取超时时间，如果无效则使用默认值
	timeout := time.Duration(cfg.Ollama.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second // 默认 5 分钟
	}

	model := cfg.Ollama.DefaultModel // 从配置中获取默认模型

	return &OllamaClient{
		url: cfg.Ollama.URL, // 从配置中获取 Ollama URL
		client: &http.Client{
			Timeout: timeout, // 设置 HTTP 请求超时
			// 配置共享传输层，包含连接池
			Transport: &http.Transport{
				MaxIdleConns:        100,              // 最大空闲连接数
				MaxIdleConnsPerHost: 10,               // 每个主机的最大空闲连接数
				IdleConnTimeout:     90 * time.Second, // 空闲连接超时时间
			},
		},
		model: model, // 设置默认模型
		cfg:   cfg,   // 存储配置
	}
}

// contextKey 是一个私有类型，用于防止 Context 键冲突
type contextKey string

const modelContextKey contextKey = "llm_model"

// WithModel 返回一个新的 Context，其中包含指定的模型名称
// 允许在运行时动态切换模型
func WithModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, modelContextKey, model)
}

// CallWithContext 是非流式调用的实现
// ctx: 上下文，可包含追踪信息和动态模型选择
// promptMessages: 对话消息历史
// tools: 可用工具的元数据
func (o *OllamaClient) CallWithContext(ctx context.Context, promptMessages []ChatMessage, tools any) (*ChatResponse, error) {
	ctx, span := tracer.Start(ctx, "OllamaClient.CallWithContext",
		trace.WithAttributes(
			attribute.String("ollama.url", o.url),
			attribute.Int("messages.count", len(promptMessages)),
		),
	)
	defer span.End()

	// 从 Context 中获取模型，如果存在则覆盖默认模型
	model := o.model
	if m, ok := ctx.Value(modelContextKey).(string); ok && m != "" {
		model = m
	}
	span.SetAttributes(attribute.String("ollama.model", model))

	Logger.Info().Str("model", model).Int("message_count", len(promptMessages)).Msg("Making API call")
	reqBody := ChatRequest{
		Model:      model,
		Messages:   promptMessages,
		Tools:      tools,
		ToolChoice: "auto",
		Stream:     false, // 明确设置为非流式
	}

	// 序列化请求体
	bs, err := json.Marshal(reqBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request marshal failed")
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, "POST", o.url, bytes.NewReader(bs))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request creation failed")
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送 HTTP 请求
	resp, err := o.client.Do(req)
	if err != nil {
		Logger.Error().Err(err).Msg("HTTP request to Ollama failed")
		span.RecordError(err)
		span.SetStatus(codes.Error, "http request failed")
		return nil, err
	}
	defer resp.Body.Close()

	// 处理非 2xx 状态码的响应
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("ollama error: %d %s", resp.StatusCode, string(body))
		span.RecordError(err)
		span.SetStatus(codes.Error, "ollama returned error status")
		return nil, err
	}

	var finalResponse ChatResponse
	// 反序列化响应体
	if err := json.NewDecoder(resp.Body).Decode(&finalResponse); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "response decode failed")
		return nil, fmt.Errorf("failed to decode ollama response: %w", err)
	}

	// 后处理：处理不一致的工具调用格式
	// 如果模型返回了内容但没有明确的 tool_calls 字段，尝试从内容中提取
	if len(finalResponse.Choices) > 0 {
		choice := &finalResponse.Choices[0]
		if len(choice.Message.ToolCalls) == 0 && choice.Message.Content != "" {
			if toolCalls := o.extractToolCalls(choice.Message.Content); len(toolCalls) > 0 {
				choice.Message.ToolCalls = toolCalls
				choice.Message.Content = "" // 如果提取到工具调用，则清空内容
			}
		}
	}

	span.SetStatus(codes.Ok, "LLM call successful")
	return &finalResponse, nil
}

// extractToolCalls 从文本内容中提取工具调用信息
// 这是一个备用机制，用于处理 LLM 返回的工具调用格式不规范的情况
func (o *OllamaClient) extractToolCalls(content string) []ToolCall {
	content = strings.TrimSpace(content)

	// 移除可能的 Markdown 代码块标记
	if strings.HasPrefix(content, "```json") {
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimSuffix(content, "```")
	} else if strings.HasPrefix(content, "```") {
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
	}
	content = strings.TrimSpace(content)

	var toolCalls []ToolCall

	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")

	if start >= 0 && end > start {
		jsonStr := content[start : end+1]

		var toolCallMap map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &toolCallMap); err == nil {
			if name, ok := toolCallMap["name"].(string); ok {
				if params, ok := toolCallMap["parameters"]; ok {
					tc := ToolCall{Type: "function"}
					tc.Function.Name = name
					if paramsMap, ok := params.(map[string]interface{}); ok {
						tc.Function.Arguments = paramsMap
					}
					toolCalls = append(toolCalls, tc)
					return toolCalls
				}
				if args, ok := toolCallMap["arguments"]; ok {
					tc := ToolCall{Type: "function"}
					tc.Function.Name = name
					if argsMap, ok := args.(map[string]interface{}); ok {
						tc.Function.Arguments = argsMap
					}
					toolCalls = append(toolCalls, tc)
					return toolCalls
				}
			}
		}
	}

	return toolCalls
}

// StreamCallWithContext 是流式调用的实现
// ctx: 上下文
// promptMessages: 对话消息历史
// tools: 可用工具的元数据
// writer: 用于写入流式响应的 io.Writer
func (o *OllamaClient) StreamCallWithContext(ctx context.Context, promptMessages []ChatMessage, tools any, writer io.Writer) error {
	ctx, span := tracer.Start(ctx, "OllamaClient.StreamCallWithContext",
		trace.WithAttributes(
			attribute.String("ollama.url", o.url),
			attribute.Int("messages.count", len(promptMessages)),
		),
	)
	defer span.End()

	// 从 Context 中获取模型，如果存在则覆盖默认模型
	model := o.model
	if m, ok := ctx.Value(modelContextKey).(string); ok && m != "" {
		model = m
	}
	span.SetAttributes(attribute.String("ollama.model", model))

	reqBody := ChatRequest{
		Model:      model,
		Messages:   promptMessages,
		Tools:      tools,
		ToolChoice: "auto",
		Stream:     true, // 明确设置为流式
	}

	// 序列化请求体
	bs, err := json.Marshal(reqBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request marshal failed")
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, "POST", o.url, bytes.NewReader(bs))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request creation failed")
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送 HTTP 请求
	resp, err := o.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "http request failed")
		return err
	}
	defer resp.Body.Close()

	// 处理非 2xx 状态码的响应
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("ollama error: %d %s", resp.StatusCode, string(body))
		span.RecordError(err)
		span.SetStatus(codes.Error, "ollama returned error status")
		return err
	}

	// 将响应体直接复制到 writer，实现流式传输
	_, err = io.Copy(writer, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "stream copy failed")
		return err
	}

	span.SetStatus(codes.Ok, "LLM stream call successful")
	return nil
}

// Embed 获取文本的向量表示
// ctx: 上下文
// text: 需要生成嵌入的文本
func (o *OllamaClient) Embed(ctx context.Context, text string) ([]float64, error) {
	ctx, span := tracer.Start(ctx, "OllamaClient.Embed",
		trace.WithAttributes(
			attribute.String("ollama.url", o.url),
			attribute.Int("text.length", len(text)),
		),
	)
	defer span.End()

	// 从配置中获取嵌入模型和 API 路径
	embedModel := o.cfg.Embedding.Model
	embedAPIPath := o.cfg.Embedding.APIPath

	// 构建嵌入 API 的完整 URL
	baseURL, err := url.Parse(o.url)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base ollama url: %w", err)
	}
	baseURL.Path = embedAPIPath
	embedURL := baseURL.String()

	reqBody := map[string]interface{}{
		"model":  embedModel,
		"prompt": text,
	}

	// 序列化请求体
	bs, err := json.Marshal(reqBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "embed request marshal failed")
		return nil, fmt.Errorf("failed to marshal embed request: %w", err)
	}

	// 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, "POST", embedURL, bytes.NewReader(bs))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "embed request creation failed")
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送 HTTP 请求
	resp, err := o.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "embed http request failed")
		return nil, err
	}
	defer resp.Body.Close()

	// 处理非 2xx 状态码的响应
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("ollama embed error: %d %s", resp.StatusCode, string(body))
		span.RecordError(err)
		span.SetStatus(codes.Error, "ollama embed returned error status")
		return nil, err
	}

	var result struct {
		Embedding []float64 `json:"embedding"` // 嵌入向量
	}
	// 反序列化响应体
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "embed response decode failed")
		return nil, err
	}
	span.SetStatus(codes.Ok, "Embedding successful")
	return result.Embedding, nil
}
