// agent 包中的工具函数模块，包含：
// - 所有内置工具的定义和实现 (WebSearchTool, RunCodeTool, etc.)
// - 工具所需的底层执行逻辑 (RunCodeSandbox, ReadFile, etc.)
// - 工具调用的验证逻辑 (isReasonableToolCall, etc.)
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// =================================================================================
//
//	Tool Argument Structs
//
// =================================================================================
type RunCodeArgs struct {
	Language string            `json:"language"`          // 编程语言 (e.g., 'python', 'go')
	Code     string            `json:"code"`              // 要执行的源代码
	Files    map[string]string `json:"files,omitempty"`   // 需要写入沙箱的额外文件
	Timeout  int               `json:"timeout,omitempty"` // 执行超时时间（秒）
}

type ReadFileArgs struct {
	Path      string `json:"path"`                 // 文件路径
	ChunkSize int    `json:"chunk_size,omitempty"` // 读取块大小
	Offset    int64  `json:"offset,omitempty"`     // 读取偏移量
}

type WriteFileArgs struct {
	Path    string `json:"path"`           // 文件路径
	Content string `json:"content"`        // 要写入的内容
	Mode    string `json:"mode,omitempty"` // 写入模式 ('overwrite' or 'append')
}

type GitCmdArgs struct {
	Workdir string   `json:"workdir"` // git 命令的工作目录
	Cmd     []string `json:"cmd"`     // git 命令及其参数
}

// =================================================================================
//
//	Tool Validation Logic (Final Version)
//
// =================================================================================

// isSimpleGreeting 检查提示是否为常见的简短问候语
func isSimpleGreeting(prompt string) bool {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if len(prompt) > 30 { // 问候语通常很短
		return false
	}
	greetings := []string{"hello", "hi", "hey", "你好", "您好", "你好啊", "在吗"}
	for _, g := range greetings {
		if prompt == g {
			return true
		}
	}
	return false
}

// isReasonableToolCall 应用一组硬编码规则来防止明显的工具幻觉
// 这是工具调用验证的唯一真实来源
func (a *Agent) isReasonableToolCall(originalPrompt string, toolCall ToolCall) bool {
	// 规则 1：对于简单的问候语，从不使用任何工具
	if isSimpleGreeting(originalPrompt) {
		Logger.Warn().Str("tool_name", toolCall.Function.Name).Str("prompt", originalPrompt).Msg("Tool call rejected by simple greeting rule.")
		return false
	}

	// 规则 2：工具必须与配置中提示的关键词相关
	prompt := strings.ToLower(originalPrompt)
	toolName := toolCall.Function.Name

	requiredKeywords, ok := a.config.ToolValidation.Keywords[toolName]
	if !ok {
		// 如果工具不在配置中，我们可以严格拒绝它
		Logger.Warn().Str("tool_name", toolName).Msg("Tool call rejected because the tool itself is not in the validation config.")
		return false
	}

	for _, kw := range requiredKeywords {
		if strings.Contains(prompt, kw) {
			return true // 找到相关关键词，因此调用可能是合理的
		}
	}

	// 如果未找到相关关键词，则工具调用不合理
	Logger.Warn().Str("tool_name", toolName).Str("prompt", originalPrompt).Msg("Tool call rejected by keyword validation.")
	return false
}

// isValidQuery 对搜索查询进行基本验证
func isValidQuery(q string) bool {
	return len(q) >= 2 &&
		strings.ContainsAny(q, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
}

// =================================================================================
//
//	Tool Implementations
//
// =================================================================================

type WebSearchTool struct{}

func (t *WebSearchTool) Name() string { return "web_search" }
func (t *WebSearchTool) Description() string {
	return "When you need to get real-time information, statistics, or search the web, use this tool. For example: CEO names, population statistics, latest news, etc."
}
func (t *WebSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "The search query."},
			"num_results": map[string]any{"type": "integer", "description": "Number of results to return."},
			"fetch_pages": map[string]any{"type": "boolean", "description": "Whether to fetch the full content of result pages."},
		},
		"required": []string{"query"},
	}
}
func (t *WebSearchTool) IsSensitive() bool { return false }
func (t *WebSearchTool) Run(ctx context.Context, argsJSON string, _ string, _ *Agent, events chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.WebSearch")
	defer span.End()

	var args WebSearchArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	span.SetAttributes(attribute.String("query", args.Query))

	if !isValidQuery(args.Query) {
		return "Error: The search query is too short or invalid.", nil
	}
	results, err := WebSearch(args)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, res := range results {
		sb.WriteString(fmt.Sprintf("Title: %s\nLink: %s\nSnippet: %s\n\n", res.Title, res.Link, res.Snippet))
	}

	return sb.String(), nil
}

type RunCodeTool struct{}

func (t *RunCodeTool) Name() string { return "run_code" }
func (t *RunCodeTool) Description() string {
	return "Executes code in a sandboxed environment. Use this ONLY when the user explicitly asks to run or execute a piece of code."
}
func (t *RunCodeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"language": map[string]any{"type": "string", "description": "The programming language (e.g., 'python', 'go')."},
			"code":     map[string]any{"type": "string", "description": "The source code to execute."},
			"timeout":  map[string]any{"type": "integer", "description": "Execution timeout in seconds."},
		},
		"required": []string{"language", "code"},
	}
}
func (t *RunCodeTool) IsSensitive() bool { return true }
func (t *RunCodeTool) Run(ctx context.Context, argsJSON string, _ string, a *Agent, events chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.RunCode")
	defer span.End()

	var args RunCodeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	span.SetAttributes(attribute.String("language", args.Language))

	// 创建一个 io.Writer，将沙箱输出转发到 events 通道
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		defer pipeWriter.Close()
		scanner := bufio.NewScanner(pipeReader)
		for scanner.Scan() {
			events <- StreamEvent{Type: "tool_output", Payload: ToolOutputEventPayload{ToolName: t.Name(), Output: scanner.Text()}}
		}
		if err := scanner.Err(); err != nil {
			Logger.Error().Err(err).Str("tool_name", t.Name()).Msg("Error reading from sandbox output pipe")
		}
	}()

	result, err := a.RunCodeSandbox(args, pipeWriter)
	if err != nil {
		return "", err
	}
	return result, nil
}

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Reads the content of a file. Use this when the user asks to see, open, or read a specific file."
}
func (t *ReadFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "The path to the file."},
		},
		"required": []string{"path"},
	}
}
func (t *ReadFileTool) IsSensitive() bool { return false }
func (t *ReadFileTool) Run(ctx context.Context, argsJSON string, _ string, _ *Agent, _ chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.ReadFile")
	defer span.End()

	var args ReadFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	span.SetAttributes(attribute.String("path", args.Path))

	return ReadFile(args), nil
}

type WriteFileTool struct{}

func (t *WriteFileTool) Name() string { return "write_file" }
func (t *WriteFileTool) Description() string {
	return "Writes content to a file. Use this ONLY when the user explicitly asks to save, write, or create a file."
}
func (t *WriteFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "The path to the file."},
			"content": map[string]any{"type": "string", "description": "The content to write."},
			"mode":    map[string]any{"type": "string", "description": "Write mode: 'overwrite' or 'append'."},
		},
		"required": []string{"path", "content"},
	}
}
func (t *WriteFileTool) IsSensitive() bool { return true }
func (t *WriteFileTool) Run(ctx context.Context, argsJSON string, _ string, _ *Agent, _ chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.WriteFile")
	defer span.End()

	var args WriteFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	span.SetAttributes(attribute.String("path", args.Path), attribute.String("mode", args.Mode))

	return WriteFile(args), nil
}

type GitCmdTool struct{}

func (t *GitCmdTool) Name() string { return "git_cmd" }
func (t *GitCmdTool) Description() string {
	return "Executes a git command in the working directory. Only safe, read-only commands are allowed."
}
func (t *GitCmdTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"workdir": map[string]any{"type": "string", "description": "The working directory for the git command."},
			"cmd":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"workdir", "cmd"},
	}
}
func (t *GitCmdTool) IsSensitive() bool { return false }
func (t *GitCmdTool) Run(ctx context.Context, argsJSON string, _ string, _ *Agent, _ chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.GitCmd")
	defer span.End()

	var args GitCmdArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	span.SetAttributes(attribute.String("workdir", args.Workdir), attribute.StringSlice("cmd", args.Cmd))

	return GitCmd(args), nil
}

type CreateSessionTool struct{}

func (t *CreateSessionTool) Name() string { return "create_session" }
func (t *CreateSessionTool) Description() string {
	return "Creates a new conversation session. Use this ONLY when the user explicitly asks to create or start a new session/topic."
}
func (t *CreateSessionTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "description": "The title for the new session."},
		},
		"required": []string{"title"},
	}
}
func (t *CreateSessionTool) IsSensitive() bool { return false }
func (t *CreateSessionTool) Run(ctx context.Context, argsJSON string, _ string, a *Agent, _ chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.CreateSession")
	defer span.End()

	var args map[string]string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	title := args["title"]
	span.SetAttributes(attribute.String("title", title))

	newSessionID := uuid.New().String()
	a.mem.CreateSession(newSessionID, title)
	return fmt.Sprintf("New session created: %s (ID: %s)", title, newSessionID), nil
}

type SwitchSessionTool struct{}

func (t *SwitchSessionTool) Name() string { return "switch_session" }
func (t *SwitchSessionTool) Description() string {
	return "Switches to a different conversation session. Use this ONLY when the user explicitly asks to switch, change, or load a different session."
}
func (t *SwitchSessionTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{"type": "string", "description": "The ID of the session to switch to."},
		},
		"required": []string{"session_id"},
	}
}
func (t *SwitchSessionTool) IsSensitive() bool { return false }
func (t *SwitchSessionTool) Run(ctx context.Context, argsJSON string, _ string, a *Agent, _ chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.SwitchSession")
	defer span.End()

	var args map[string]string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	targetID := args["session_id"]
	span.SetAttributes(attribute.String("target_session_id", targetID))

	if a.mem.SetCurrentSession(targetID) {
		msgs, _ := a.mem.GetSessionMessages(targetID)
		return fmt.Sprintf("Switched to session ID: %s, which contains %d messages.", targetID, len(msgs)), nil
	}
	return fmt.Sprintf("Could not switch to session ID: %s. Session not found.", targetID), nil
}

type KnowledgeSearchTool struct{}

func (t *KnowledgeSearchTool) Name() string { return "knowledge_search" }
func (t *KnowledgeSearchTool) Description() string {
	return "Searches the local knowledge base. Use this when the user asks about project documentation, history, or specific domain knowledge."
}
func (t *KnowledgeSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "The query to search for in the knowledge base."},
			"top_k": map[string]any{"type": "integer", "description": "The number of top results to return."},
		},
		"required": []string{"query"},
	}
}
func (t *KnowledgeSearchTool) IsSensitive() bool { return false }
func (t *KnowledgeSearchTool) Run(ctx context.Context, argsJSON string, _ string, a *Agent, _ chan<- StreamEvent) (string, error) {
	_, span := tracer.Start(ctx, "Tool.KnowledgeSearch")
	defer span.End()

	var args struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}
	if args.TopK <= 0 {
		args.TopK = 3
	}
	span.SetAttributes(attribute.String("query", args.Query), attribute.Int("top_k", args.TopK))

	queryVec, err := a.llm.Embed(ctx, args.Query)
	if err != nil {
		return "", fmt.Errorf("embed error: %v", err)
	}

	results, err := a.vectorStore.Search(queryVec, args.TopK)
	if err != nil {
		return "", fmt.Errorf("vector search error: %v", err)
	}
	if len(results) == 0 {
		return "No relevant knowledge found.", nil
	}

	var sb strings.Builder
	for i, res := range results {
		sb.WriteString(fmt.Sprintf("[%d] (Similarity: %.2f)\n%s\n\n", i+1, res.Score, res.Doc.Content))
	}
	return sb.String(), nil
}

// =================================================================================
//
//	Multi-Agent Tools
//
// =================================================================================

type CallCoderTool struct{}

func (t *CallCoderTool) Name() string { return "call_coder" }
func (t *CallCoderTool) Description() string {
	return "Delegates a coding task to the Coder Agent. Use this for writing, modifying, or reviewing code."
}
func (t *CallCoderTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":        map[string]any{"type": "string", "description": "The type of task, e.g., 'string'"},
					"description": map[string]any{"type": "string", "description": "The coding task to be performed."},
				},
				"required": []string{"type", "description"},
			},
		},
		"required": []string{"task"},
	}
}
func (t *CallCoderTool) IsSensitive() bool { return false }
func (t *CallCoderTool) Run(ctx context.Context, argsJSON string, _ string, a *Agent, events chan<- StreamEvent) (string, error) {
	var args struct {
		Task struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}

	Logger.Info().Str("foreman_agent", a.role).Str("coder_task", args.Task.Description).Msg("Foreman calling Coder Agent")

	coderAgent, ok := a.otherAgents["coder"]
	if !ok {
		Logger.Error().Str("foreman_agent", a.role).Msg("Coder agent not found in otherAgents map")
		return "", fmt.Errorf("coder agent not found")
	}

	subAgentEvents := make(chan StreamEvent)
	go coderAgent.StreamRunWithSessionAndImages(ctx, args.Task.Description, "", nil, "", subAgentEvents)

	var finalAnswer strings.Builder
	for event := range subAgentEvents {
		// 将子 Agent 的所有事件转发到 Foreman 的 events 通道
		events <- event

		// 同时收集最终答案或错误
		if event.Type == "token" {
			if p, ok := event.Payload.(TokenEventPayload); ok {
				finalAnswer.WriteString(p.Text)
			}
		} else if event.Type == "error" {
			if p, ok := event.Payload.(ErrorEventPayload); ok {
				Logger.Error().Str("coder_agent_error", p.Message).Msg("Coder Agent returned an error")
				return "", fmt.Errorf("coder agent error: %s", p.Message)
			}
		}
	}

	Logger.Info().Str("foreman_agent", a.role).Str("coder_result_preview", truncateString(finalAnswer.String(), 100)).Msg("Coder Agent returned result")
	return finalAnswer.String(), nil
}

type CallResearcherTool struct{}

func (t *CallResearcherTool) Name() string { return "call_researcher" }
func (t *CallResearcherTool) Description() string {
	return "Delegates a research task to the Researcher Agent. Use this for searching the web or knowledge base."
}
func (t *CallResearcherTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":        map[string]any{"type": "string", "description": "The type of task, e.g., 'string'"},
					"description": map[string]any{"type": "string", "description": "The research task to be performed."},
				},
				"required": []string{"type", "description"},
			},
		},
		"required": []string{"task"},
	}
}
func (t *CallResearcherTool) IsSensitive() bool { return false }
func (t *CallResearcherTool) Run(ctx context.Context, argsJSON string, _ string, a *Agent, events chan<- StreamEvent) (string, error) {
	var args struct {
		Task struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid args: %v", err)
	}

	Logger.Info().Str("foreman_agent", a.role).Str("researcher_task", args.Task.Description).Msg("Foreman calling Researcher Agent")

	researcherAgent, ok := a.otherAgents["researcher"]
	if !ok {
		Logger.Error().Str("foreman_agent", a.role).Msg("Researcher agent not found in otherAgents map")
		return "", fmt.Errorf("researcher agent not found")
	}

	subAgentEvents := make(chan StreamEvent)
	go researcherAgent.StreamRunWithSessionAndImages(ctx, args.Task.Description, "", nil, "", subAgentEvents)

	var finalAnswer strings.Builder
	for event := range subAgentEvents {
		// 将子 Agent 的所有事件转发到 Foreman 的 events 通道
		events <- event

		// 同时收集最终答案或错误
		if event.Type == "token" {
			if p, ok := event.Payload.(TokenEventPayload); ok {
				finalAnswer.WriteString(p.Text)
			}
		} else if event.Type == "error" {
			if p, ok := event.Payload.(ErrorEventPayload); ok {
				Logger.Error().Str("researcher_agent_error", p.Message).Msg("Researcher Agent returned an error")
				return "", fmt.Errorf("researcher agent error: %s", p.Message)
			}
		}
	}

	Logger.Info().Str("foreman_agent", a.role).Str("researcher_result_preview", truncateString(finalAnswer.String(), 100)).Msg("Researcher Agent returned result")
	return finalAnswer.String(), nil
}

// =================================================================================
//
//	Underlying Tool Logic
//
// =================================================================================

var (
	cleanupMu    sync.Mutex
	workDirs     = make(map[string]time.Time)
	cleanupTimer *time.Timer
)

func init() {
	// 这个 init 函数在 main 之前运行
	// 我们不能在这里访问 Agent.config，所以 cleanupTimer 需要不同的管理方式
	// 或者确保 ensureSandboxInitialized 稍后被调用
	// 现在，我们保持简单，并假设 cleanupWorkDirs 会被定期调用
	cleanupTimer = time.AfterFunc(1*time.Hour, cleanupWorkDirs)
}

func (a *Agent) ensureSandboxInitialized() {
	a.sandboxOnce.Do(func() {
		// 检查 docker 是否正在运行
		cmd := exec.Command("docker", "info")
		if err := cmd.Run(); err != nil {
			Logger.Error().Err(err).Msg("Docker is not running or not installed. Code execution will fail.")
		}

		maxConcurrency := a.config.Sandbox.MaxConcurrency
		if maxConcurrency <= 0 {
			maxConcurrency = 5
		}
		a.runCodeSandboxSemaphore = make(chan struct{}, maxConcurrency)
	})
}

func cleanupWorkDirs() {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	now := time.Now()
	for workDir, createTime := range workDirs {
		if now.Sub(createTime) > 1*time.Hour {
			os.RemoveAll(workDir)
			delete(workDirs, workDir)
		}
	}
	cleanupTimer.Reset(1 * time.Hour)
}

func (a *Agent) RunCodeSandbox(args RunCodeArgs, stream io.Writer) (string, error) {
	// 在执行开始时添加检查
	cmdCheck := exec.Command("docker", "info")
	if err := cmdCheck.Run(); err != nil {
		errMsg := "Docker is not running or accessible. Please start Docker Desktop and try again."
		Logger.Error().Err(err).Msg(errMsg)
		return errMsg, fmt.Errorf(errMsg)
	}

	a.ensureSandboxInitialized()
	a.runCodeSandboxSemaphore <- struct{}{}
	defer func() { <-a.runCodeSandboxSemaphore }()

	tmp := fmt.Sprintf("agent_work_%d", time.Now().UnixNano())
	base := filepath.Join("./sandboxes", tmp)
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", fmt.Errorf("mkdir error: %v", err)
	}

	cleanupMu.Lock()
	workDirs[base] = time.Now()
	cleanupMu.Unlock()

	mainFile := ""
	switch args.Language {
	case "python":
		mainFile = "main.py"
		if err := os.WriteFile(filepath.Join(base, mainFile), []byte(args.Code), 0644); err != nil {
			return "", fmt.Errorf("write file error: %v", err)
		}
	case "go":
		mainFile = "main.go"
		if err := os.WriteFile(filepath.Join(base, mainFile), []byte(args.Code), 0644); err != nil {
			return "", fmt.Errorf("write file error: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, "go.mod"), []byte("module sandbox\n\ngo 1.20\n"), 0644); err != nil {
			return "", fmt.Errorf("write go.mod error: %v", err)
		}
	default:
		mainFile = "main.txt"
		if err := os.WriteFile(filepath.Join(base, mainFile), []byte(args.Code), 0644); err != nil {
			return "", fmt.Errorf("write file error: %v", err)
		}
	}

	for p, content := range args.Files {
		full := filepath.Join(base, p)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return "", fmt.Errorf("mkdir error: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("write file error: %v", err)
		}
	}

	timeout := a.config.Sandbox.DefaultTimeout
	if args.Timeout > 0 && args.Timeout < a.config.Sandbox.MaxTimeout {
		timeout = args.Timeout
	}

	image := "python:3.11"
	cmdSh := ""
	switch args.Language {
	case "python":
		cmdSh = fmt.Sprintf("timeout %d python3 %s", timeout, mainFile)
	case "go":
		cmdSh = fmt.Sprintf("timeout %d go run .", timeout)
	default:
		cmdSh = fmt.Sprintf("timeout %d cat %s", timeout, mainFile)
		image = "alpine:3.18"
	}

	dockerArgs := []string{
		"run", "--rm",
		"-v", fmt.Sprintf("%s:/work", base),
		"-w", "/work",
		"--network", "none",
		"--pids-limit", "64",
		"--memory", fmt.Sprintf("%dm", a.config.Sandbox.MemoryMB),
		"--cpus", fmt.Sprintf("%.2f", a.config.Sandbox.CpuQuota),
		image,
		"sh", "-lc", cmdSh,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout+3)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)

	var combinedOutput bytes.Buffer
	multiWriter := io.MultiWriter(&combinedOutput, stream)
	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter

	err := cmd.Run()

	go func() {
		time.Sleep(1 * time.Minute)
		os.RemoveAll(base)
		cleanupMu.Lock()
		delete(workDirs, base)
		cleanupMu.Unlock()
	}()

	if err != nil {
		return combinedOutput.String(), fmt.Errorf("error: %v\noutput:\n%s", err, combinedOutput.String())
	}
	return combinedOutput.String(), nil
}

func ReadFile(args ReadFileArgs) string {
	info, err := os.Stat(args.Path)
	if err != nil {
		return "read error: " + err.Error()
	}
	if info.IsDir() {
		return "read error: path is a directory"
	}
	if info.Size() > 10*1024*1024 {
		return "read error: file too large (max 10MB)"
	}

	file, err := os.Open(args.Path)
	if err != nil {
		return "read error: " + err.Error()
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)

	if args.Offset > 0 {
		if _, err := file.Seek(args.Offset, 0); err != nil {
			return "seek error: " + err.Error()
		}
	}

	if args.ChunkSize > 0 {
		if args.ChunkSize > 10*1024*1024 {
			args.ChunkSize = 10 * 1024 * 1024
		}
		buffer := make([]byte, args.ChunkSize)
		n, err := reader.Read(buffer)
		if n > 0 {
			return string(buffer[:n])
		}
		if err != nil && err != io.EOF {
			return "chunk read error: " + err.Error()
		}
		return ""
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return "read all error: " + err.Error()
	}
	return string(content)
}

func WriteFile(args WriteFileArgs) string {
	mode := args.Mode
	if mode == "" {
		mode = "overwrite"
	}
	if filepath.IsAbs(args.Path) {
		return "write error: absolute path not allowed"
	}
	if len(args.Content) > 10*1024*1024 {
		return "write error: content too large (max 10MB)"
	}

	if mode == "overwrite" {
		if err := os.MkdirAll(filepath.Dir(args.Path), 0755); err != nil {
			return "write error: " + err.Error()
		}
		if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
			return "write error: " + err.Error()
		}
		return "written"
	}

	f, err := os.OpenFile(args.Path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return "append error: " + err.Error()
	}
	defer f.Close()
	if _, err := f.WriteString(args.Content); err != nil {
		return "append write error: " + err.Error()
	}
	return "appended"
}

func GitCmd(args GitCmdArgs) string {
	if args.Workdir == "" {
		return "git error: workdir empty"
	}
	if _, err := os.Stat(args.Workdir); os.IsNotExist(err) {
		return "git error: workdir not exists"
	}
	if len(args.Cmd) == 0 {
		return "git error: cmd empty"
	}

	allowedCommands := map[string]bool{
		"status": true, "log": true, "diff": true, "show": true, "blame": true,
		"rev-parse": true, "branch": true, "tag": true, "remote": true,
		"config": true, "ls-files": true,
	}
	if !allowedCommands[args.Cmd[0]] {
		return fmt.Sprintf("git error: command '%s' not allowed", args.Cmd[0])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args.Cmd...)
	cmd.Dir = args.Workdir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git error: %v\noutput:\n%s", err, string(out))
	}
	return string(out)
}

func MarshalArgs(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
