package agent

// StreamEvent 表示代理执行流中的单个事件。
// 这些事件用于实时向客户端（例如 WebSocket 或 SSE 连接）发送代理的思考过程、工具调用、输出和最终响应。
type StreamEvent struct {
	Type    string      `json:"type"`              // 事件类型，例如 "thinking", "tool_start", "tool_output", "token", "final_answer", "error", "awaiting_confirmation"
	Payload interface{} `json:"payload,omitempty"` // 与事件关联的数据负载，具体类型取决于 Type 字段
}

// ThinkingEventPayload 是 "thinking" 事件的负载结构。
// 用于通知客户端代理正在进行思考或处理。
type ThinkingEventPayload struct {
	Text string `json:"text"` // 思考或处理的文本描述
}

// ToolCallEventPayload 是 "tool_start" 和 "tool_end" 事件的负载结构。
// 用于通知客户端工具的开始和结束执行。
type ToolCallEventPayload struct {
	ToolName  string                 `json:"tool_name"` // 工具的名称
	Arguments map[string]interface{} `json:"arguments"` // 工具调用的参数
}

// ToolOutputEventPayload 是 "tool_output" 事件的负载结构。
// 用于实时向客户端发送工具执行过程中的输出。
type ToolOutputEventPayload struct {
	ToolName string `json:"tool_name"` // 产生输出的工具名称
	Output   string `json:"output"`    // 工具的输出内容
}

// TokenEventPayload 是 "token" 事件的负载结构。
// 用于实时向客户端发送大语言模型生成的文本 token。
type TokenEventPayload struct {
	Text string `json:"text"` // 生成的文本 token
}

// FinalAnswerEventPayload 是 "final_answer" 事件的负载结构。
// 用于通知客户端代理已生成最终答案。
type FinalAnswerEventPayload struct {
	Text string `json:"text"` // 最终答案的文本内容
}

// ErrorEventPayload 是 "error" 事件的负载结构。
// 用于通知客户端代理执行过程中发生了错误。
type ErrorEventPayload struct {
	Message string `json:"message"` // 错误消息
}

// AwaitingConfirmationEventPayload 是 "awaiting_confirmation" 事件的负载结构。
// 用于通知客户端代理正在等待用户确认敏感工具的执行。
type AwaitingConfirmationEventPayload struct {
	ConfirmationID string                 `json:"confirmation_id"` // 确认请求的唯一 ID
	ToolName       string                 `json:"tool_name"`       // 需要确认的工具名称
	Arguments      map[string]interface{} `json:"arguments"`       // 工具调用的参数
}
