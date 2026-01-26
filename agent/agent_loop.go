// agent_loop.go
// agent 包包含AI代理的核心逻辑，包括：
// - Agent结构体：协调Ollama调用与工具执行
// - 代理执行循环：实现ReAct模式的推理流程
package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Agent 结构体代表一个AI代理实例，负责协调以下组件：
// llm: 与大语言模型通信的客户端（抽象接口）
// mem: 会话记忆存储（用于持久化对话历史）
// prompts: 提示词管理器
// vectorStore: 向量存储（用于RAG）
// maxIterations: 代理执行循环的最大迭代次数
// toolRegistry: 工具注册表，管理所有可用工具
// confirmationManager: 工具执行确认管理器
// config: 应用程序配置
// sandboxOnce: 用于确保沙箱初始化只执行一次
// runCodeSandboxSemaphore: 用于控制代码沙箱并发执行的信号量
type Agent struct {
	llm                     LLMProvider
	mem                     *MemoryV3
	prompts                 *PromptManager
	vectorStore             VectorStore // 使用接口类型
	maxIterations           int
	toolRegistry            *ToolRegistry
	confirmationManager     *ConfirmationManager
	config                  Config
	sandboxOnce             sync.Once
	runCodeSandboxSemaphore chan struct{}
}

// NewAgent 创建新的代理实例
// l: LLMProvider 接口实现
// m: MemoryV3 实例
// vs: VectorStore 接口实现
// cfg: 应用程序配置
func NewAgent(l LLMProvider, m *MemoryV3, vs VectorStore, cfg Config) *Agent {
	a := &Agent{
		llm:                 l,
		mem:                 m,
		prompts:             NewPromptManager(""),
		vectorStore:         vs,
		maxIterations:       cfg.Agent.MaxIterations,
		toolRegistry:        NewToolRegistry(),
		confirmationManager: NewConfirmationManager(),
		config:              cfg,
	}
	a.registerDefaultTools() // 注册默认工具
	return a
}

// registerDefaultTools 注册系统默认工具到代理的工具注册表中
func (a *Agent) registerDefaultTools() {
	a.toolRegistry.Register(&WebSearchTool{})
	a.toolRegistry.Register(&RunCodeTool{})
	a.toolRegistry.Register(&ReadFileTool{})
	a.toolRegistry.Register(&WriteFileTool{})
	a.toolRegistry.Register(&GitCmdTool{})
	a.toolRegistry.Register(&CreateSessionTool{})
	a.toolRegistry.Register(&SwitchSessionTool{})
	a.toolRegistry.Register(&KnowledgeSearchTool{})
}

// GetMemory 获取Agent的内存实例
func (a *Agent) GetMemory() *MemoryV3 {
	return a.mem
}

// GetVectorStore 获取Agent的向量存储实例
func (a *Agent) GetVectorStore() VectorStore {
	return a.vectorStore
}

// GetLLM 获取Agent的LLMProvider实例
func (a *Agent) GetLLM() LLMProvider {
	return a.llm
}

// GetPromptManager 获取Agent的PromptManager实例
func (a *Agent) GetPromptManager() *PromptManager {
	return a.prompts
}

// GetConfirmationManager 获取Agent的ConfirmationManager实例
func (a *Agent) GetConfirmationManager() *ConfirmationManager {
	return a.confirmationManager
}

// validateToolCall 验证工具调用的合理性
// 它首先进行硬编码的启发式检查，然后通过 LLM 进行二次验证
func (a *Agent) validateToolCall(ctx context.Context, originalPrompt string, toolCall ToolCall) bool {
	ctx, span := tracer.Start(ctx, "Agent.validateToolCall")
	defer span.End()

	// --- 硬编码启发式检查 (快速路径) ---
	// 使用 Agent 实例的方法进行检查
	if !a.isReasonableToolCall(originalPrompt, toolCall) {
		Logger.Warn().Str("tool", toolCall.Function.Name).Msg("Tool call rejected by heuristic check")
		return false
	}
	// --- 启发式检查结束 ---

	Logger.Info().Msg("Tool call passed heuristic checks, proceeding to LLM validation.")
	args, _ := json.Marshal(toolCall.Function.Arguments)
	// 渲染工具验证提示
	prompt, err := a.prompts.Render("tool_validation", map[string]string{
		"OriginalPrompt": originalPrompt,
		"ToolName":       toolCall.Function.Name,
		"ToolArgs":       string(args),
	})
	if err != nil {
		Logger.Error().Err(err).Msg("Failed to render tool validation prompt")
		return true // 失败开放：如果无法渲染提示，则假定调用有效
	}

	validationMessages := []ChatMessage{{Role: "user", Content: prompt}}
	// 调用 LLM 进行验证
	resp, err := a.llm.CallWithContext(ctx, validationMessages, nil)
	if err != nil {
		Logger.Error().Err(err).Msg("Tool validation LLM call failed")
		return true // 失败开放
	}

	if len(resp.Choices) > 0 {
		answer := strings.TrimSpace(resp.Choices[0].Message.Content)
		Logger.Info().Str("validation_answer", answer).Msg("Tool validation response")
		// 如果 LLM 回复包含 "yes" 或 "是"，则认为工具调用有效
		return strings.Contains(strings.ToLower(answer), "yes") || strings.Contains(answer, "是")
	}

	return true // 失败开放
}

// prepareSessionAndMessages 初始化会话并加载历史消息
// 如果 sessionID 为空，则创建新会话；否则切换到指定会话
func (a *Agent) prepareSessionAndMessages(prompt string, sessionID string, images []string) (string, []ChatMessage) {
	if sessionID == "" {
		sessionID = a.mem.GetCurrentSessionID()
	}
	if sessionID == "" {
		sessionID = uuid.New().String()
		a.mem.CreateSession(sessionID, fmt.Sprintf("会话-%s", time.Now().Format("2006-01-02 15:04:05")))
	} else {
		a.mem.SetCurrentSession(sessionID)
	}

	var messages []ChatMessage
	if msgs, exists := a.mem.GetSessionMessages(sessionID); exists {
		messages = msgs
	}
	if len(messages) == 0 {
		systemContent := a.prompts.GetSystemPrompt()
		messages = []ChatMessage{{Role: "system", Content: systemContent}}
	}

	userMsg := ChatMessage{Role: "user", Content: prompt, Images: images}
	messages = append(messages, userMsg)
	a.mem.AddMessageToSession(sessionID, userMsg)
	a.mem.AddConversation(prompt)

	return sessionID, messages
}

// processLLMStream 处理 LLM 的流式响应，提取文本内容和工具调用
func (a *Agent) processLLMStream(ctx context.Context, messages []ChatMessage, events chan<- StreamEvent) (string, []ToolCall, error) {
	toolsMetadata := a.toolRegistry.GetMetadata() // 获取所有工具的元数据
	pipeReader, pipeWriter := io.Pipe()           // 创建管道用于 LLM 响应的流式处理

	// 发送“正在思考”事件给前端
	events <- StreamEvent{Type: "thinking", Payload: ThinkingEventPayload{Text: "正在思考如何响应..."}}

	// 在 goroutine 中调用 LLM 的流式接口，将响应写入管道
	go func() {
		defer pipeWriter.Close()
		err := a.llm.StreamCallWithContext(ctx, messages, toolsMetadata, pipeWriter)
		if err != nil {
			Logger.Error().Err(err).Msg("LLM Stream call failed")
			errorEvent := StreamEvent{Type: "error", Payload: ErrorEventPayload{Message: err.Error()}}
			errBytes, _ := json.Marshal(errorEvent)
			pipeWriter.Write(errBytes) // 将错误事件写入管道
		}
	}()

	var fullContent strings.Builder // 存储完整的文本内容
	var allToolCalls []ToolCall     // 存储所有提取到的工具调用

	scanner := bufio.NewScanner(pipeReader) // 使用扫描器从管道读取数据
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event StreamEvent
		// 尝试解析为 StreamEvent，如果解析成功且是错误事件，则直接转发
		if err := json.Unmarshal(line, &event); err == nil && event.Type == "error" {
			events <- event
			return "", nil, fmt.Errorf("stream error: %v", event.Payload)
		}
		var chunk map[string]interface{}
		// 尝试解析为通用 JSON 块
		if err := json.Unmarshal(line, &chunk); err != nil {
			Logger.Warn().Bytes("line", line).Msg("Failed to unmarshal stream chunk")
			continue
		}
		// 提取消息内容和工具调用
		if message, ok := chunk["message"].(map[string]interface{}); ok {
			if content, ok := message["content"].(string); ok && content != "" {
				fullContent.WriteString(content)
			}
			if toolCallsRaw, ok := message["tool_calls"].([]interface{}); ok {
				for _, tcRaw := range toolCallsRaw {
					tcBytes, _ := json.Marshal(tcRaw)
					var tc ToolCall
					if err := json.Unmarshal(tcBytes, &tc); err == nil {
						if tc.Type == "" {
							tc.Type = "function" // 默认为函数类型
						}
						allToolCalls = append(allToolCalls, tc)
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		Logger.Error().Err(err).Msg("Error reading from LLM stream pipe")
		events <- StreamEvent{Type: "error", Payload: ErrorEventPayload{Message: "Stream read error"}}
		return "", nil, err
	}

	// 备用提取：如果 LLM 没有明确返回 tool_calls 字段，但内容中包含类似 JSON 的结构，尝试从中提取
	if len(allToolCalls) == 0 && strings.Contains(fullContent.String(), `"name"`) {
		Logger.Info().Msg("Attempting fallback tool extraction")
		extractedCalls := extractToolCallsFromContent(fullContent.String())
		if len(extractedCalls) > 0 {
			allToolCalls = extractedCalls
			Logger.Info().Int("count", len(allToolCalls)).Msg("Fallback extraction successful")
		} else {
			Logger.Warn().Str("content", fullContent.String()).Msg("Fallback extraction failed")
		}
	}

	return fullContent.String(), allToolCalls, nil
}

// handleToolCalls 并发执行工具调用并返回结果
func (a *Agent) handleToolCalls(ctx context.Context, toolCalls []ToolCall, sessionID string, events chan<- StreamEvent) []ChatMessage {
	var wg sync.WaitGroup
	toolResults := make(chan ChatMessage, len(toolCalls)) // 使用带缓冲的 channel 存储工具结果

	for _, toolCall := range toolCalls {
		wg.Add(1)
		go func(tc ToolCall) {
			defer wg.Done()

			// --- 工具确认逻辑 ---
			tool, exists := a.toolRegistry.Get(tc.Function.Name)
			if exists && tool.IsSensitive() { // 如果工具是敏感的，需要用户确认
				// 注册确认请求，获取确认 ID 和结果通道
				confID, ch := a.confirmationManager.RegisterRequest()

				// 发送事件到前端，请求用户确认
				events <- StreamEvent{
					Type: "awaiting_confirmation",
					Payload: AwaitingConfirmationEventPayload{
						ConfirmationID: confID,
						ToolName:       tc.Function.Name,
						Arguments:      tc.Function.Arguments,
					},
				}

				// 等待用户响应
				allowed := <-ch
				if !allowed { // 如果用户拒绝
					events <- StreamEvent{Type: "thinking", Payload: ThinkingEventPayload{Text: "用户拒绝了工具执行请求。"}}
					toolResults <- ChatMessage{Role: "tool", Content: "User denied the execution of this tool.", Name: tc.Function.Name}
					return
				}
			}
			// --- 工具确认逻辑结束 ---

			// 发送工具开始执行事件
			events <- StreamEvent{Type: "tool_start", Payload: ToolCallEventPayload{ToolName: tc.Function.Name, Arguments: tc.Function.Arguments}}
			argsBytes, _ := json.Marshal(tc.Function.Arguments)
			fc := &FunctionCall{Name: tc.Function.Name, Arguments: argsBytes}

			toolPipeReader, toolPipeWriter := io.Pipe() // 创建管道用于工具的流式输出
			var toolResult string
			var toolErr error
			var execWg sync.WaitGroup

			execWg.Add(1)
			// 在 goroutine 中执行工具
			go func() {
				defer execWg.Done()
				toolResult, toolErr = a.execTool(ctx, fc, sessionID, toolPipeWriter)
			}()

			// 扫描工具的流式输出并转发给前端
			toolOutputScanner := bufio.NewScanner(toolPipeReader)
			for toolOutputScanner.Scan() {
				outputChunk := toolOutputScanner.Text()
				events <- StreamEvent{Type: "tool_output", Payload: ToolOutputEventPayload{ToolName: tc.Function.Name, Output: outputChunk}}
			}
			execWg.Wait() // 等待工具执行完成

			// 发送工具结束执行事件
			events <- StreamEvent{Type: "tool_end", Payload: ToolCallEventPayload{ToolName: tc.Function.Name}}
			if toolErr != nil {
				toolResult = fmt.Sprintf("Tool '%s' execution failed.\nError: %v", tc.Function.Name, toolErr)
			}
			toolResults <- ChatMessage{Role: "tool", Content: toolResult, Name: tc.Function.Name}
		}(toolCall)
	}
	wg.Wait() // 等待所有工具执行完成
	close(toolResults)

	var results []ChatMessage
	for res := range toolResults {
		results = append(results, res)
	}
	return results
}

// StreamRunWithSessionAndImages 是代理处理流式请求的主循环
// 它实现了 ReAct 模式，通过迭代调用 LLM、验证工具、执行工具来生成响应
func (a *Agent) StreamRunWithSessionAndImages(ctx context.Context, prompt string, sessionID string, images []string, model string, events chan<- StreamEvent) {
	defer close(events) // 确保事件通道在函数退出时关闭
	defer func() {
		// 确保“完成”事件总是被发送
		events <- StreamEvent{Type: "status", Payload: map[string]string{"status": "stream_complete"}}
	}()

	// 启动 OpenTelemetry Span 进行追踪
	ctx, span := tracer.Start(ctx, "Agent.StreamRunWithSessionAndImages",
		trace.WithAttributes(
			attribute.String("prompt", prompt),
			attribute.String("session_id", sessionID),
			attribute.String("model", model),
			attribute.Int("images_count", len(images)),
		),
	)
	defer span.End() // 确保 Span 在函数退出时结束

	Logger.Info().Str("prompt", prompt).Int("image_count", len(images)).Str("model", model).Msg("User prompt received")

	// 准备会话和消息历史
	sessionID, messages := a.prepareSessionAndMessages(prompt, sessionID, images)

	// 如果指定了模型，则将其添加到上下文中
	if model != "" {
		ctx = WithModel(ctx, model)
	}

	var lastToolCallHash string // 用于检测重复的工具调用
	// 代理执行循环
	for iter := 0; iter < a.maxIterations; iter++ {
		continueLoop, newMessages := a._runIteration(ctx, prompt, sessionID, messages, &lastToolCallHash, events)
		messages = newMessages
		if !continueLoop { // 如果 _runIteration 返回 false，表示循环结束
			break
		}
	}

	// 如果迭代次数达到上限，设置 Span 状态为错误
	if span.IsRecording() {
		span.SetStatus(codes.Error, "Iteration limit reached")
	}
	// 发送错误事件
	events <- StreamEvent{Type: "error", Payload: ErrorEventPayload{Message: "Iteration limit reached"}}
}

// _runIteration 执行代理循环的单次迭代
// 返回一个布尔值，指示是否继续循环，以及更新后的消息列表
func (a *Agent) _runIteration(ctx context.Context, prompt, sessionID string, messages []ChatMessage, lastToolCallHash *string, events chan<- StreamEvent) (bool, []ChatMessage) {
	ctx, span := tracer.Start(ctx, "Agent._runIteration")
	defer span.End()

	// 1. 调用 LLM 获取响应
	fullContent, allToolCalls, err := a.processLLMStream(ctx, messages, events)
	if err != nil {
		return false, messages
	}

	msg := ChoiceMessage{Role: "assistant", Content: fullContent, ToolCalls: allToolCalls}

	Logger.Info().Int("tool_calls", len(msg.ToolCalls)).Str("content_preview", truncateString(msg.Content, 50)).Msg("LLM response processed")

	// 2. 如果 LLM 建议工具调用
	if len(msg.ToolCalls) > 0 {
		// 发送“正在验证工具”事件
		events <- StreamEvent{Type: "thinking", Payload: ThinkingEventPayload{Text: "检测到工具调用，正在验证并准备执行..."}}

		// 验证工具调用的合理性
		if !a.validateToolCall(ctx, prompt, msg.ToolCalls[0]) {
			Logger.Warn().Interface("tool_call", msg.ToolCalls[0]).Msg("Tool call failed validation. Forcing text response.")
			// 如果验证失败，强制 LLM 返回文本响应
			forceTextPrompt, _ := a.prompts.Render("force_text_response", nil)
			messages = append(messages, ChatMessage{Role: "assistant", ToolCalls: msg.ToolCalls})
			messages = append(messages, ChatMessage{Role: "user", Content: forceTextPrompt})
			return true, messages // 继续循环，让 LLM 重新生成响应
		}

		// 检测重复工具调用，防止无限循环
		currentToolCallHash := hashToolCalls(msg.ToolCalls)
		if currentToolCallHash == *lastToolCallHash {
			Logger.Warn().Str("hash", currentToolCallHash).Msg("Detected duplicate tool call. Breaking loop.")
			// 如果检测到重复，强制 LLM 总结答案
			forceFinalAnswerMsg, _ := a.prompts.Render("duplicate_tool_call", nil)
			messages = append(messages, ChatMessage{Role: "user", Content: forceFinalAnswerMsg})
			return true, messages // 继续循环，让 LLM 总结答案
		}
		*lastToolCallHash = currentToolCallHash

		// 将助手的工具调用消息添加到消息历史
		assistantMsg := ChatMessage{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls}
		messages = append(messages, assistantMsg)
		a.mem.AddMessageToSession(sessionID, assistantMsg)

		// 执行工具调用
		toolResults := a.handleToolCalls(ctx, msg.ToolCalls, sessionID, events)
		// 发送“工具执行完毕”事件
		events <- StreamEvent{Type: "thinking", Payload: ThinkingEventPayload{Text: "工具执行完毕，正在处理工具结果..."}}

		// 将工具执行结果添加到消息历史
		for _, res := range toolResults {
			messages = append(messages, res)
			a.mem.AddMessageToSession(sessionID, res)
		}
		return true, messages // 继续循环，将工具结果反馈给 LLM
	}

	// 3. 如果 LLM 返回文本内容，则认为是最终答案
	if msg.Content != "" {
		// 发送“正在生成最终答案”事件和文本 token
		events <- StreamEvent{Type: "thinking", Payload: ThinkingEventPayload{Text: "正在生成最终答案..."}}
		events <- StreamEvent{Type: "token", Payload: TokenEventPayload{Text: msg.Content}}
	}
	lastAnswer := msg.Content
	a.mem.AddNote(lastAnswer) // 记录最终答案
	assistantMsg := ChatMessage{Role: "assistant", Content: lastAnswer}
	a.mem.AddMessageToSession(sessionID, assistantMsg) // 将最终答案添加到消息历史

	// 设置 Span 状态为成功
	if span.IsRecording() {
		span.SetStatus(codes.Ok, "Agent finished successfully")
	}
	return false, messages // 循环结束
}

// hashToolCalls 计算工具调用的哈希值，用于检测重复的工具调用
func hashToolCalls(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	// 仅对第一个工具调用进行哈希，简化处理
	call := calls[0]
	data, _ := json.Marshal(call)
	hasher := sha256.New()
	hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil))
}

// extractToolCallsFromContent 是一个辅助函数，用于从字符串内容中查找并提取工具调用 JSON
// 这是一个备用机制，用于处理 LLM 返回的工具调用格式不规范的情况
func extractToolCallsFromContent(content string) []ToolCall {
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
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		jsonStr := content[start : end+1]

		// 1. 尝试解析为标准 ToolCall 结构
		var tc ToolCall
		if err := json.Unmarshal([]byte(jsonStr), &tc); err == nil && tc.Function.Name != "" {
			if tc.Type == "" {
				tc.Type = "function"
			}
			return []ToolCall{tc}
		}

		// 2. 尝试解析为扁平结构 (name/arguments 在顶层)
		var flatCall struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &flatCall); err == nil && flatCall.Name != "" {
			return []ToolCall{
				{
					Type: "function",
					Function: ToolCallFunction{
						Name:      flatCall.Name,
						Arguments: flatCall.Arguments,
					},
				},
			}
		}

		// 3. 尝试解析为 ToolCall 列表 (不常见，作为最终备用)
		var tcs []ToolCall
		if err := json.Unmarshal([]byte(jsonStr), &tcs); err == nil && len(tcs) > 0 {
			for i := range tcs {
				if tcs[i].Type == "" {
					tcs[i].Type = "function"
				}
			}
			return tcs
		}
	}
	return nil
}

// execTool 执行指定的工具函数
func (a *Agent) execTool(ctx context.Context, fc *FunctionCall, sessionID string, stream io.Writer) (string, error) {
	ctx, span := tracer.Start(ctx, "Agent.execTool",
		trace.WithAttributes(
			attribute.String("tool.name", fc.Name),
			attribute.String("session_id", sessionID),
			attribute.String("tool.arguments", string(fc.Arguments)),
		),
	)
	defer span.End()
	fname := fc.Name
	Logger.Info().Str("tool_name", fname).Msg("Executing tool")
	tool, exists := a.toolRegistry.Get(fname) // 从工具注册表中获取工具
	if !exists {
		err := fmt.Errorf("model hallucinated an unknown tool: %s", fname)
		span.SetStatus(codes.Error, err.Error())
		return err.Error(), nil // 将错误作为结果返回给 LLM
	}
	// 运行工具
	res, err := tool.Run(ctx, string(fc.Arguments), sessionID, a, stream)
	if err != nil {
		Logger.Error().Err(err).Str("tool_name", fname).Msg("Tool execution failed")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	span.SetStatus(codes.Ok, "Tool executed successfully")
	return res, nil
}

// truncateString 截断字符串到指定长度，并在末尾添加 "..."
func truncateString(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
