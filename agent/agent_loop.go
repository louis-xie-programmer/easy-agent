// agent 包包含AI代理的核心逻辑，包括：
// - Agent结构体：协调Ollama调用与工具执行
// - 工具函数元数据定义：声明可用的外部工具能力
// - 代理执行循环：实现ReAct模式的推理流程
package agent

import (
	"encoding/json"
	"fmt"
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

// Tool metadata (helps model decide). Keep limited and documented.
// toolsMetadata 返回工具函数的元数据描述
// 这些描述帮助大语言模型理解可用工具及其参数
// 返回值：JSON格式的工具数组，符合OpenAI工具调用规范
func toolsMetadata() any {
	return []map[string]any{
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
	}
}

// Run handles a user prompt and returns final agent answer.
// Run 方法执行完整的代理工作流
// 实现ReAct模式：接收用户输入 -> 调用LLM -> 执行工具 -> 处理结果
// 支持最多6次迭代的思维链推理
// 参数：prompt 用户输入的自然语言提示
// 返回值：最终回答和错误信息
func (a *Agent) Run(prompt string) (string, error) {
	// initial messages
	messages := []ChatMessage{
		{Role: "system", Content: "你是 AI 编程伙伴，资深的go编程专家，擅长审查代码、写测试、运行沙箱代码以及生成修复建议。需要调用工具时，请使用 function_call（JSON）。"},
		{Role: "user", Content: prompt},
	}
	a.mem.AddConversation(prompt)

	var lastAnswer string // 存储最后一次成功的回复内容
	// 最多允许6次迭代，防止无限循环
	for iter := 0; iter < 6; iter++ {
		// 本地Ollama不支持工具调用，暂时禁用tools参数
		tollsMetadata := toolsMetadata()
		cr, err := a.ollama.Call(messages, tollsMetadata)
		if err != nil {
			return "", err
		}
		if len(cr.Choices) == 0 {
			return "", fmt.Errorf("no choices from model")
		}
		msg := cr.Choices[0].Message

		// 检测到函数调用请求，需要执行外部工具
		// 将模型消息添加到上下文，并执行相应工具
		// 工具输出将以tool角色返回给模型进行下一步推理
		// if function call present -> execute tool
		if msg.FunctionCall != nil {
			// append model message to messages
			messages = append(messages, ChatMessage{Role: "assistant", Content: "", Name: msg.Name})
			// route tool
			res := a.execTool(msg.FunctionCall)
			// append tool output as tool role
			messages = append(messages, ChatMessage{Role: "tool", Content: res, Name: msg.FunctionCall.Name})
			// continue loop so model sees tool output
			continue
		}

		// 模型直接返回最终答案，无需工具调用
		// 记录回答到记忆并返回结果
		// normal assistant reply
		lastAnswer = msg.Content
		// add note to memory
		a.mem.AddNote(lastAnswer)
		return lastAnswer, nil
	}
	// 达到最大迭代次数仍未获得最终答案
	return lastAnswer, fmt.Errorf("iteration limit reached")
}

func (a *Agent) execTool(fc *FunctionCall) string {
	fname := fc.Name
	switch fname {
	case "run_code":
		var args RunCodeArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return RunCodeSandbox(args)
	case "read_file":
		var args ReadFileArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return ReadFile(args)
	case "write_file":
		var args WriteFileArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return WriteFile(args)
	case "git_cmd":
		var args GitCmdArgs
		_ = json.Unmarshal(fc.Arguments, &args)
		return GitCmd(args)
	default:
		return "unknown tool: " + fname
	}
}
