// ollama.go
// agent 包中的Ollama客户端模块，负责：
// - 定义与Ollama兼容的API请求/响应结构体
// - 实现HTTP通信逻辑
// - 处理认证和错误情况
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama-compatible request/response minimal types
// ChatMessage 表示对话中的一条消息
// 符合OpenAI API的消息格式规范
// Role: 角色（system/user/assistant/tool）
// Content: 消息内容文本
// Name: 工具调用时的函数名称
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
}

// ChatRequest 封装发送给Ollama模型的完整请求
// Model: 使用的模型名称
// Messages: 对话历史消息数组
// Tools: 可用工具的元数据描述
// ToolChoice: 工具选择策略（auto/manual/none）
type ChatRequest struct {
	Model      string        `json:"model"`
	Messages   []ChatMessage `json:"messages"`
	Tools      any           `json:"tools,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
	Stream     bool          `json:"stream,omitempty"` // 添加流式支持
}

// FunctionCall 表示模型建议执行的函数调用
// Name: 函数名称
// Arguments: JSON格式的参数字符串
// 该结构用于实现LLM的工具调用能力
type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ChoiceMessage 表示模型返回的选择结果
// 包含角色、内容以及可能的函数调用指令
// 与ChatMessage类似但包含FunctionCall字段
// 用于解析模型的响应
// ChoiceMessage 结构体
// 注意：这是Ollama API返回格式的一部分
type ChoiceMessage struct {
	Role         string        `json:"role"`
	Content      string        `json:"content,omitempty"`
	FunctionCall *FunctionCall `json:"function_call,omitempty"`
	Name         string        `json:"name,omitempty"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"` // 添加对tool_calls的支持
}

// ToolCall 表示模型建议执行的工具调用
// Name: 工具名称
// Arguments: 工具参数
type ToolCall struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// Choice 表示一个完整的响应选项
// 包含一条Message
// Ollama API可能返回多个选择，当前只处理第一个
type Choice struct {
	Message      ChoiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

// ChatResponse 表示从Ollama接收到的完整响应
// Choices: 响应选项数组（通常只有一个）
// 该结构用于反序列化API返回的JSON数据
type ChatResponse struct {
	Choices []Choice `json:"choices"`
	// Ollama may include other fields
}

// OllamaClient 封装与Ollama服务的通信
// url: API端点URL
// client: HTTP客户端实例
// model: 使用的模型名称
// 提供统一的接口来调用大语言模型
type OllamaClient struct {
	url    string
	client *http.Client
	model  string
}

// NewOllamaClient 创建新的Ollama客户端实例
// 参数：
//
//	url: Ollama服务的API端点
//	timeout: HTTP请求超时时间
//
// 默认使用支持工具调用的deepseek-r1模型
// 返回值：初始化的OllamaClient指针
func NewOllamaClient(url string, timeout time.Duration) *OllamaClient {
	// 增加最小超时时间，确保至少有90秒的处理时间
	if timeout < 90*time.Second {
		timeout = 90 * time.Second
	}

	return &OllamaClient{
		url: url,
		client: &http.Client{
			Timeout: timeout,
			// 添加连接池配置
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		model: "qwen3-vl:4b", // 使用支持工具调用的模型
	}
}

// Call 方法向Ollama服务发送聊天请求
// 参数：
//
//	promptMessages: 对话历史消息
//	tools: 工具元数据（可选）
//
// 返回值：API响应和错误信息
// 实现完整的HTTP请求-响应流程，包括：
// - 请求序列化
// - HTTP头设置
// - 响应处理
// - 错误转换
func (o *OllamaClient) Call(promptMessages []ChatMessage, tools any) (*ChatResponse, error) {
	LogAsync("INFO", fmt.Sprintf("发起API调用，消息数量: %d", len(promptMessages)))
	reqBody := ChatRequest{
		Model:      o.model,
		Messages:   promptMessages,
		Tools:      tools,
		ToolChoice: "auto",
		Stream:     false, // 非流式调用
	}

	// 将请求体转换为JSON
	bs, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// 创建HTTP请求
	req, err := http.NewRequest("POST", o.url, bytes.NewReader(bs))
	if err != nil {
		return nil, err
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	// Ollama本地安装不需要API密钥
	// 已移除认证头设置
	// If Ollama requires any API key header, set it via env and uncomment:
	// req.Header.Set("Authorization", "Bearer "+os.Getenv("OLLAMA_API_KEY"))

	// 不要创建新的超时上下文，使用传入的上下文
	// 如果没有上下文，则使用客户端默认超时
	// 检查请求是否已经有上下文
	/*if req.Context() == context.Background() {
		// 只有在没有上下文时才应用默认超时
		ctx, cancel := context.WithTimeout(context.Background(), o.client.Timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}*/

	resp, err := o.client.Do(req)
	if err != nil {
		LogAsync("ERROR", fmt.Sprintf("HTTP request to Ollama failed: %v", err))
		return nil, err
	}
	defer resp.Body.Close()

	// 检查HTTP状态码
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama error: %d %s", resp.StatusCode, string(body))
	}

	// Ollama /api/chat 返回的是 application/x-ndjson 流式响应
	// 需要逐行解析每个 JSON 对象
	decoder := json.NewDecoder(resp.Body)
	var finalResponse ChatResponse

	for {
		var chunk map[string]interface{}
		if err := decoder.Decode(&chunk); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("failed to decode ollama response chunk: %w", err)
		}

		// 检查是否有错误信息
		if errorMsg, ok := chunk["error"].(string); ok {
			return nil, fmt.Errorf("ollama error: %s", errorMsg)
		}

		// 提取 content 并累加到最终响应
		if message, ok := chunk["message"].(map[string]interface{}); ok {
			if content, ok := message["content"].(string); ok && content != "" {
				if len(finalResponse.Choices) == 0 {
					finalResponse.Choices = append(finalResponse.Choices, Choice{
						Message: ChoiceMessage{Role: "assistant", Content: ""},
					})
				}
				finalResponse.Choices[0].Message.Content += content

				// 检查是否包含工具调用（文本形式）
				if toolCalls := o.extractToolCalls(content); len(toolCalls) > 0 {
					finalResponse.Choices[0].Message.ToolCalls = toolCalls
				}
			}

			// 检查是否是结束标记
			if done, ok := chunk["done"].(bool); ok && done {
				if finishReason, ok := chunk["finish_reason"].(string); ok {
					if len(finalResponse.Choices) == 0 {
						finalResponse.Choices = append(finalResponse.Choices, Choice{})
					}
					finalResponse.Choices[0].FinishReason = finishReason
				}
				break
			}
		}
	}

	if len(finalResponse.Choices) == 0 {
		return nil, fmt.Errorf("empty response from ollama")
	}

	return &finalResponse, nil
}

// extractToolCalls 从文本内容中提取工具调用信息
func (o *OllamaClient) extractToolCalls(content string) []ToolCall {
	// 查找类似 {"name": "...", "parameters": {...}} 的模式
	// 这是deepseek-r1-tool-calling模型返回工具调用的方式

	// 简单的启发式方法：查找JSON对象
	var toolCalls []ToolCall

	// 查找第一个 '{' 和最后一个 '}'
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")

	if start >= 0 && end > start {
		jsonStr := content[start : end+1]

		// 尝试解析为工具调用
		var toolCallMap map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &toolCallMap); err == nil {
			if name, ok := toolCallMap["name"].(string); ok {
				// 检查是否有parameters字段
				if params, ok := toolCallMap["parameters"]; ok {
					toolCall := ToolCall{
						Name:      name,
						Arguments: make(map[string]interface{}),
					}

					// 将parameters转换为map[string]interface{}
					if paramsMap, ok := params.(map[string]interface{}); ok {
						toolCall.Arguments = paramsMap
					}

					toolCalls = append(toolCalls, toolCall)
				}
			}
		}
	}

	return toolCalls
}

// StreamCall 实时流式处理Ollama响应
// 直接将content流式写入writer，避免内容重组
// 参数：
//
//	writer - 实现io.Writer接口的目标流（如WebSocket连接）
func (o *OllamaClient) StreamCall(promptMessages []ChatMessage, tools any, writer io.Writer) error {
	reqBody := ChatRequest{
		Model:      o.model,
		Messages:   promptMessages,
		Tools:      tools,
		ToolChoice: "auto",
		Stream:     true, // 启用流式调用
	}

	bs, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", o.url, bytes.NewReader(bs))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	// 不要创建新的超时上下文，使用传入的上下文
	// 如果没有上下文，则使用客户端默认超时
	// 检查请求是否已经有上下文
	/*if req.Context() == context.Background() {
		// 只有在没有上下文时才应用默认超时
		ctx, cancel := context.WithTimeout(context.Background(), o.client.Timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}*/

	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama error: %d %s", resp.StatusCode, string(body))
	}

	decoder := json.NewDecoder(resp.Body)
	for {
		var chunk map[string]interface{}
		if err := decoder.Decode(&chunk); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("failed to decode ollama response chunk: %w", err)
		}

		if errorMsg, ok := chunk["error"].(string); ok {
			return fmt.Errorf("ollama error: %s", errorMsg)
		}

		if message, ok := chunk["message"].(map[string]interface{}); ok {
			if content, ok := message["content"].(string); ok && content != "" {
				if _, err := writer.Write([]byte(content)); err != nil {
					return err
				}
			}
		}

		// 检查是否是结束标记
		if done, ok := chunk["done"].(bool); ok && done {
			break
		}
	}
	return nil
}
