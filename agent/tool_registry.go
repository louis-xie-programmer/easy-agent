package agent

import (
	"context"
	"sync"
)

// Tool 定义了工具的通用接口。所有可供 AI 代理使用的工具都必须实现此接口。
type Tool interface {
	// Name 返回工具的唯一名称，例如 "web_search"。
	Name() string
	// Description 返回工具的详细描述，用于在 Prompt 中告诉大语言模型该工具的作用和使用场景。
	Description() string
	// Schema 返回工具参数的 JSON Schema 定义，用于指导大语言模型生成正确的工具调用参数。
	Schema() map[string]any
	// IsSensitive 返回一个布尔值，指示该工具的操作是否敏感，需要用户进行二次确认。
	IsSensitive() bool
	// Run 执行工具的实际逻辑。
	// ctx: 包含追踪信息和取消信号的上下文。
	// argsJSON: 大语言模型生成的 JSON 格式参数字符串。
	// sessionID: 当前会话的唯一标识符，某些工具可能需要会话上下文。
	// agent: Agent 实例的引用，允许工具反向调用 Agent 的其他能力（例如，创建新会话、访问内存或向量存储）。
	// events: 用于流式写入工具执行过程中的事件。
	// 返回工具执行的结果字符串和可能发生的错误。
	Run(ctx context.Context, argsJSON string, sessionID string, agent *Agent, events chan<- StreamEvent) (string, error)
}

// ToolRegistry 管理所有可用工具的注册和查找。
type ToolRegistry struct {
	tools map[string]Tool // 存储工具名称到工具实例的映射
	mu    sync.RWMutex    // 读写互斥锁，用于保护 tools 映射的并发访问
}

// NewToolRegistry 创建并返回一个新的 ToolRegistry 实例。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]Tool), // 初始化工具映射
	}
}

// Register 注册一个新工具到注册表中。
// t: 要注册的 Tool 接口实例。
func (r *ToolRegistry) Register(t Tool) {
	r.mu.Lock() // 获取写锁，确保并发安全
	defer r.mu.Unlock()
	r.tools[t.Name()] = t // 将工具添加到映射中，以其名称作为键
}

// Get 根据工具名称从注册表中获取工具实例。
// name: 要查找的工具名称。
// 返回找到的 Tool 实例和指示是否找到的布尔值。
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock() // 获取读锁，允许多个读取者并发访问
	defer r.mu.RUnlock()
	t, ok := r.tools[name] // 从映射中查找工具
	return t, ok
}

// GetMetadata 生成所有注册工具的元数据列表，这些元数据将提供给大语言模型，
// 以便模型了解可用的工具及其功能。
// 返回一个包含所有工具元数据的 map 列表，每个 map 描述一个工具。
func (r *ToolRegistry) GetMetadata() []map[string]any {
	r.mu.RLock() // 获取读锁
	defer r.mu.RUnlock()

	var metadata []map[string]any
	for _, t := range r.tools {
		// 为每个工具构建符合 LLM 工具调用规范的元数据结构
		metadata = append(metadata, map[string]any{
			"type": "function", // 工具类型，通常为 "function"
			"function": map[string]any{
				"name":        t.Name(),        // 工具名称
				"description": t.Description(), // 工具描述
				"parameters":  t.Schema(),      // 工具参数的 JSON Schema
			},
		})
	}
	return metadata
}
