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
}

// Choice 表示一个完整的响应选项
// 包含一条Message
// Ollama API可能返回多个选择，当前只处理第一个
type Choice struct {
	Message ChoiceMessage `json:"message"`
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
//   url: Ollama服务的API端点
//   timeout: HTTP请求超时时间
// 默认使用deepseek-r1:1.5b模型
// 返回值：初始化的OllamaClient指针
func NewOllamaClient(url string, timeout time.Duration) *OllamaClient {
	return &OllamaClient{
		url:    url,
		client: &http.Client{Timeout: timeout},
		model:  "deepseek-r1:1.5b",
	}
}

// Call 方法向Ollama服务发送聊天请求
// 参数：
//   promptMessages: 对话历史消息
//   tools: 工具元数据（可选）
// 返回值：API响应和错误信息
// 实现完整的HTTP请求-响应流程，包括：
// - 请求序列化
// - HTTP头设置
// - 响应处理
// - 错误转换
func (o *OllamaClient) Call(promptMessages []ChatMessage, tools any) (*ChatResponse, error) {
	reqBody := ChatRequest{
		Model:    o.model,
		Messages: promptMessages,
		Tools:      tools,
		ToolChoice: "auto",
	}
	// 将请求体转换为JSON
	bs, _ := json.Marshal(reqBody)
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

	resp, err := o.client.Do(req)
	if err != nil {
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
			}
		}
	}

	if len(finalResponse.Choices) == 0 {
		return nil, fmt.Errorf("empty response from ollama")
	}

	return &finalResponse, nil
}
