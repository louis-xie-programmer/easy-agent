// agent_loop.go
// agent 包包含AI代理的核心逻辑，包括：
// - Agent结构体：协调Ollama调用与工具执行
// - 工具函数元数据定义：声明可用的外部工具能力
// - 代理执行循环：实现ReAct模式的推理流程
package agent

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"
)

// Agent orchestrates calls
// Agent 结构体代表一个AI代理实例，负责协调以下组件：
// ollama: 与大语言模型通信的客户端
// mem: 会话记忆存储（用于持久化对话历史）
type Agent struct {
	ollama *OllamaClient
	mem    *Memory
}

// NewAgent 创建新的代理实例
// 参数：
//
//	o: Ollama客户端，用于与LLM通信
//	m: 内存存储，用于保存对话状态
//
// 返回值：初始化的Agent指针
func NewAgent(o *OllamaClient, m *Memory) *Agent {
	return &Agent{ollama: o, mem: m}
}

// GetMemory 获取Agent的内存实例
func (a *Agent) GetMemory() *Memory {
	return a.mem
}

// Tool metadata (helps model decide). Keep limited and documented.
// toolsMetadata 返回工具函数的元数据描述
// 这些描述帮助大语言模型理解可用工具及其参数
// 返回值：JSON格式的工具数组，符合OpenAI工具调用规范
func toolsMetadata() any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "web_search",
				"description": "进行互联网搜索并返回 topN 结果，可选抓取页面正文。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query":       map[string]any{"type": "string"},
						"num_results": map[string]any{"type": "integer"},
						"fetch_pages": map[string]any{"type": "boolean"},
						"timeout":     map[string]any{"type": "integer"},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "run_code",
				"description": "在沙箱中运行代码（语言: python/go），返回 stdout/stderr。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"language": map[string]any{"type": "string"},
						"code":     map[string]any{"type": "string"},
						"timeout":  map[string]any{"type": "integer"},
					},
					"required": []string{"language", "code"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "读取文件内容，受大小限制。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "write_file",
				"description": "写文件（谨慎使用）。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
						"mode":    map[string]any{"type": "string"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "git_cmd",
				"description": "在工作目录执行 git 操作（只允许安全命令）。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"workdir": map[string]any{"type": "string"},
						"cmd":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"workdir", "cmd"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "create_session",
				"description": "创建一个新的会话主题。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{"type": "string"},
					},
					"required": []string{"title"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "switch_session",
				"description": "切换到指定的会话主题。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string"},
					},
					"required": []string{"session_id"},
				},
			},
		},
	}
}

// Run handles a user prompt and returns final agent answer.
// Run 方法执行完整的代理工作流
// 实现ReAct模式：接收用户输入 -> 调用LLM -> 执行工具 -> 处理结果
// 支持最多6次迭代的思维链推理
// 参数：prompt 用户输入的自然语言提示
// 返回值：最终回答和错误信息
func (a *Agent) Run(prompt string) (string, error) {
	return a.RunWithSession(prompt, "")
}

// RunWithSession 在指定会话中执行代理工作流
func (a *Agent) RunWithSession(prompt string, sessionID string) (string, error) {
	LogAsync("INFO", fmt.Sprintf("User prompt: %s", prompt))

	// 如果没有提供会话ID，则使用当前会话
	if sessionID == "" {
		sessionID = a.mem.GetCurrentSessionID()
	}

	// 如果仍然没有会话ID，则创建一个新会话
	if sessionID == "" {
		sessionID = uuid.New().String()
		a.mem.CreateSession(sessionID, fmt.Sprintf("会话-%s", time.Now().Format("2006-01-02 15:04:05")))
	} else {
		// 更新当前会话的最后活动时间
		a.mem.SetCurrentSession(sessionID)
	}

	// 获取会话历史消息
	var messages []ChatMessage
	if msgs, exists := a.mem.GetSessionMessages(sessionID); exists {
		messages = msgs
	}
	// 只有当没有历史消息时才添加系统消息
	if len(messages) == 0 {
		// 初始化系统消息
		messages = []ChatMessage{
			{Role: "system", Content: "你是 AI 编程伙伴，资深的go编程专家，擅长审查代码、写测试、运行沙箱代码以及生成修复建议。需要调用工具时，请使用 function_call（JSON）。"},
		}
	}

	// 添加用户消息
	userMsg := ChatMessage{Role: "user", Content: prompt}
	messages = append(messages, userMsg)
	a.mem.AddMessageToSession(sessionID, userMsg)
	a.mem.AddConversation(prompt)

	var lastAnswer string // 存储最后一次成功的回复内容
	// 最多允许6次迭代，防止无限循环
	for iter := 0; iter < 6; iter++ {
		// 首先尝试带工具的调用
		toolsMetadata := toolsMetadata()
		cr, err := a.ollama.Call(messages, toolsMetadata)

		// 如果是因为工具不支持导致的错误，尝试不带工具的调用
		if err != nil && strings.Contains(err.Error(), "does not support tools") {
			LogAsync("WARN", "Model does not support tools, falling back to no-tools mode")
			cr, err = a.ollama.Call(messages, nil)
		}

		if err != nil {
			LogAsync("ERROR", fmt.Sprintf("Ollama call failed: %v", err))
			return "", err
		}

		// 调试：打印模型原始响应
		// fmt.Printf("[DEBUG] Model response: %+v\n", cr)
		LogAsync("DEBUG", fmt.Sprintf("Model response: %+v", cr))
		if len(cr.Choices) == 0 {
			// fmt.Printf("[ERROR] No choices from model response\n")
			LogAsync("ERROR", "No choices from model response")
			return "", fmt.Errorf("no choices from model")
		}
		msg := cr.Choices[0].Message

		// 检测到函数调用请求，需要执行外部工具
		// 将模型消息添加到上下文，并执行相应工具
		// 工具输出将以tool角色返回给模型进行下一步推理
		// if function call present -> execute tool
		// 检查新的ToolCalls字段
		if len(msg.ToolCalls) > 0 {
			// append model message to messages
			assistantMsg := ChatMessage{Role: "assistant", Content: msg.Content}
			messages = append(messages, assistantMsg)
			a.mem.AddMessageToSession(sessionID, assistantMsg)

			// 处理所有工具调用
			for _, toolCall := range msg.ToolCalls {
				// 构造FunctionCall结构以兼容现有代码
				argsBytes, _ := json.Marshal(toolCall.Arguments)
				fc := &FunctionCall{
					Name:      toolCall.Name,
					Arguments: argsBytes,
				}

				// route tool
				res := a.execTool(fc, sessionID)
				// append tool output as tool role
				toolMsg := ChatMessage{Role: "tool", Content: res, Name: toolCall.Name}
				messages = append(messages, toolMsg)
				a.mem.AddMessageToSession(sessionID, toolMsg)
			}

			// continue loop so model sees tool output
			continue
		}

		// 检查旧的FunctionCall字段（向后兼容）
		if msg.FunctionCall != nil {
			// append model message to messages
			assistantMsg := ChatMessage{Role: "assistant", Content: msg.Content, Name: msg.Name}
			messages = append(messages, assistantMsg)
			a.mem.AddMessageToSession(sessionID, assistantMsg)

			// route tool
			res := a.execTool(msg.FunctionCall, sessionID)
			// append tool output as tool role
			toolMsg := ChatMessage{Role: "tool", Content: res, Name: msg.FunctionCall.Name}
			messages = append(messages, toolMsg)
			a.mem.AddMessageToSession(sessionID, toolMsg)

			// continue loop so model sees tool output
			continue
		}

		// 调试：打印未触发工具调用的原因
		LogAsync("DEBUG", fmt.Sprintf("No function call triggered. Message content: %s", msg.Content))

		// 模型直接返回最终答案，无需工具调用
		// 记录回答到记忆并返回结果
		// normal assistant reply
		lastAnswer = msg.Content
		// add note to memory
		a.mem.AddNote(lastAnswer)

		// 添加助手回复到会话
		assistantMsg := ChatMessage{Role: "assistant", Content: lastAnswer}
		messages = append(messages, assistantMsg)
		a.mem.AddMessageToSession(sessionID, assistantMsg)

		return lastAnswer, nil
	}
	// 达到最大迭代次数仍未获得最终答案
	return lastAnswer, fmt.Errorf("iteration limit reached")
}

func (a *Agent) execTool(fc *FunctionCall, sessionID string) string {
	fname := fc.Name
	switch fname {
	case "run_code":
		LogAsync("INFO", "Executing run_code tool")
		var args RunCodeArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return RunCodeSandbox(args)
	case "read_file":
		LogAsync("INFO", "Executing read_file tool")
		var args ReadFileArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return ReadFile(args)
	case "write_file":
		LogAsync("INFO", "Executing write_file tool")
		var args WriteFileArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return WriteFile(args)
	case "git_cmd":
		LogAsync("INFO", "Executing git_cmd tool")
		var args GitCmdArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return GitCmd(args)
	case "web_search":
		LogAsync("INFO", "Executing web_search tool")
		var args WebSearchArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		results, err := WebSearch(args)
		if err != nil {
			return "web search error: " + err.Error()
		}
		return MarshalArgs(results)
	case "create_session":
		LogAsync("INFO", "Executing create_session tool")
		var args map[string]string
		_ = json.Unmarshal(fc.Arguments, &args)
		title := args["title"]
		newSessionID := uuid.New().String()
		a.mem.CreateSession(newSessionID, title)
		return fmt.Sprintf("已创建新会话: %s (ID: %s)", title, newSessionID)
	case "switch_session":
		LogAsync("INFO", "Executing switch_session tool")
		var args map[string]string
		_ = json.Unmarshal(fc.Arguments, &args)
		targetSessionID := args["session_id"]
		if a.mem.SetCurrentSession(targetSessionID) {
			msgs, _ := a.mem.GetSessionMessages(targetSessionID)
			return fmt.Sprintf("已切换到会话 ID: %s，该会话包含 %d 条消息", targetSessionID, len(msgs))
		}
		return fmt.Sprintf("无法切换到会话 ID: %s，会话不存在", targetSessionID)
	default:
		LogAsync("ERROR", "Unknown tool: "+fname)
		return "unknown tool: " + fname
	}
}
