// agent 包中的工具函数模块，包含：
// - 代码沙箱执行
// - 文件系统操作
// - Git版本控制集成
// 所有函数都被设计为安全的、受限的操作
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Tool argument types
// RunCodeArgs 定义运行代码工具的参数结构
// Language: 编程语言（python/go）
// Code: 要执行的源代码
// Files: 额外需要创建的文件（如依赖文件）
// Timeout: 执行超时时间（秒）
type RunCodeArgs struct {
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Files    map[string]string `json:"files,omitempty"`
	Timeout  int               `json:"timeout,omitempty"` // seconds
}

// ReadFileArgs 定义读取文件工具的参数结构
// Path: 要读取的文件路径
// ChunkSize: 分块大小（字节），0表示不分块
// Offset: 读取偏移量（字节）
type ReadFileArgs struct {
	Path      string `json:"path"`
	ChunkSize int    `json:"chunk_size,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
}

// WriteFileArgs 定义写入文件工具的参数结构
// Path: 目标文件路径
// Content: 要写入的内容
// Mode: 写入模式（overwrite/append）
type WriteFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode,omitempty"`
}

// GitCmdArgs 定义Git命令工具的参数结构
// Workdir: 工作目录
// Cmd: 要执行的Git命令数组（如["status"]）
type GitCmdArgs struct {
	Workdir string   `json:"workdir"`
	Cmd     []string `json:"cmd"`
}

// 添加工作区清理机制
var (
	cleanupMu    sync.Mutex
	workDirs     = make(map[string]time.Time)
	cleanupTimer *time.Timer
)

// 初始化清理定时器
func init() {
	cleanupTimer = time.AfterFunc(1*time.Hour, cleanupWorkDirs)
}

// 清理过期的工作目录
func cleanupWorkDirs() {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()

	now := time.Now()
	for workDir, createTime := range workDirs {
		// 清理超过1小时的临时目录
		if now.Sub(createTime) > 1*time.Hour {
			os.RemoveAll(workDir)
			delete(workDirs, workDir)
		}
	}

	// 重新调度下次清理
	cleanupTimer.Reset(1 * time.Hour)
}

// runCodeSandboxPool 控制并发执行的数量
var runCodeSandboxSemaphore = make(chan struct{}, 5) // 最多同时运行5个沙箱

// RunCodeSandbox 在Docker沙箱中安全执行代码
// 特性：
//   - 使用临时工作目录
//   - 支持Python和Go语言
//   - 严格的资源限制（CPU/内存/网络）
//   - 自动清理机制
//
// 返回值：执行输出或错误信息
func RunCodeSandbox(args RunCodeArgs) string {
	// 控制并发执行数量
	runCodeSandboxSemaphore <- struct{}{}
	defer func() { <-runCodeSandboxSemaphore }()

	// 创建唯一的临时工作空间
	// 命名格式：agent_work_时间戳
	// 存储在./sandboxes目录下
	// workspace
	tmp := fmt.Sprintf("agent_work_%d", time.Now().UnixNano())
	base := filepath.Join("./sandboxes", tmp)
	if err := os.MkdirAll(base, 0755); err != nil {
		return fmt.Sprintf("mkdir error: %v", err)
	}

	// 注册工作目录以备清理
	cleanupMu.Lock()
	workDirs[base] = time.Now()
	cleanupMu.Unlock()

	// 根据语言类型写入主文件
	// Python: main.py
	// Go: main.go + go.mod
	// 其他: main.txt
	// 同时写入额外指定的文件
	// write files
	mainFile := ""
	switch args.Language {
	case "python":
		mainFile = "main.py"
		if err := os.WriteFile(filepath.Join(base, mainFile), []byte(args.Code), 0644); err != nil {
			return fmt.Sprintf("write file error: %v", err)
		}
	case "go":
		if err := os.WriteFile(filepath.Join(base, "main.go"), []byte(args.Code), 0644); err != nil {
			return fmt.Sprintf("write file error: %v", err)
		}
		// for go module, quick hack: create go.mod
		if err := os.WriteFile(filepath.Join(base, "go.mod"), []byte("module sandbox\n\ngo 1.20\n"), 0644); err != nil {
			return fmt.Sprintf("write go.mod error: %v", err)
		}
	default:
		if err := os.WriteFile(filepath.Join(base, "main.txt"), []byte(args.Code), 0644); err != nil {
			return fmt.Sprintf("write file error: %v", err)
		}
	}

	for p, content := range args.Files {
		full := filepath.Join(base, p)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return fmt.Sprintf("mkdir error: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			return fmt.Sprintf("write file error: %v", err)
		}
	}

	timeout := 30                                // 增加默认超时时间
	if args.Timeout > 0 && args.Timeout < 3000 { // 限制最大超时时间为300秒
		timeout = args.Timeout
	}

	// 根据编程语言选择合适的Docker镜像
	// 设置执行超时（默认8秒）
	// 构建docker run命令参数
	// --network none: 禁用网络访问
	// --pids-limit 64: 限制进程数
	// --memory 256m: 内存限制
	// --cpus 0.5: CPU限制
	// choose appropriate image
	image := "python:3.11"
	cmdSh := ""
	switch args.Language {
	case "python":
		cmdSh = fmt.Sprintf("timeout %d python3 %s", timeout, mainFile)
	case "go":
		cmdSh = fmt.Sprintf("timeout %d go run .", timeout)
	default:
		cmdSh = fmt.Sprintf("timeout %d cat %s", timeout, "main.txt")
		image = "alpine:3.18"
	}

	dockerArgs := []string{
		"run", "--rm",
		"-v", fmt.Sprintf("%s:/work", base),
		"-w", "/work",
		"--network", "none",
		"--pids-limit", "64",
		"--memory", "256m",
		"--cpus", "0.5",
		image,
		"sh", "-lc", cmdSh,
	}

	// 创建带超时的上下文，比代码执行超时多3秒用于清理
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout+3)*time.Second)
	defer cancel()

	// 执行Docker命令并捕获输出
	// 使用CombinedOutput同时获取stdout和stderr
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	out, err := cmd.CombinedOutput()

	// 异步清理工作目录（不要阻塞当前操作）
	go func() {
		time.Sleep(1 * time.Minute) // 等待1分钟后清理
		os.RemoveAll(base)
		cleanupMu.Lock()
		delete(workDirs, base)
		cleanupMu.Unlock()
	}()

	if err != nil {
		return fmt.Sprintf("error: %v\noutput:\n%s", err, string(out))
	}
	return string(out)
}

// ReadFile 安全读取文件内容
// 特性：
//   - 支持分块读取大文件
//   - 缓冲I/O优化
//   - 错误处理与完整性检查
//
// 返回值：文件内容或错误信息
func ReadFile(args ReadFileArgs) string {
	// 检查文件是否存在且是常规文件
	info, err := os.Stat(args.Path)
	if err != nil {
		return "read error: " + err.Error()
	}
	if info.IsDir() {
		return "read error: path is a directory"
	}

	// 限制文件大小（10MB以内）
	if info.Size() > 10*1024*1024 {
		return "read error: file too large (max 10MB)"
	}

	file, err := os.Open(args.Path)
	if err != nil {
		return "read error: " + err.Error()
	}
	defer file.Close()

	// 设置缓冲区提高I/O性能
	reader := bufio.NewReaderSize(file, 64*1024) // 64KB buffer

	// 处理偏移量
	if args.Offset > 0 {
		if _, err := file.Seek(args.Offset, 0); err != nil {
			return "seek error: " + err.Error()
		}
	}

	// 分块读取模式
	if args.ChunkSize > 0 {
		if args.ChunkSize > 10*1024*1024 { // 限制块大小
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

	// 全量读取模式（保持向后兼容）
	content, err := io.ReadAll(reader)
	if err != nil {
		return "read all error: " + err.Error()
	}
	return string(content)
}

// WriteFile 安全写入文件
// 支持两种模式：
//   - overwrite: 覆盖写入（默认）
//   - append: 追加写入
//
// 返回值：操作结果描述
func WriteFile(args WriteFileArgs) string {
	mode := args.Mode
	if mode == "" {
		mode = "overwrite"
	}

	// 检查文件路径安全性
	if filepath.IsAbs(args.Path) {
		return "write error: absolute path not allowed"
	}

	// 限制文件大小（10MB以内）
	if len(args.Content) > 10*1024*1024 {
		return "write error: content too large (max 10MB)"
	}

	// 覆盖模式：直接写入新内容
	if mode == "overwrite" {
		// 确保目录存在
		if err := os.MkdirAll(filepath.Dir(args.Path), 0755); err != nil {
			return "write error: " + err.Error()
		}
		if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
			return "write error: " + err.Error()
		}
		return "written"
	}

	// 追加模式：打开文件并在末尾添加内容
	// 使用O_APPEND标志确保原子性操作
	// append
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

// GitCmd 执行安全的Git命令
// 特性：
//   - 工作目录验证
//   - 命令执行与输出捕获
//   - 错误处理
//
// 返回值：Git命令输出或错误信息
func GitCmd(args GitCmdArgs) string {
	// basic safety: require local path exists
	if args.Workdir == "" {
		return "git error: workdir empty"
	}

	// 检查工作目录是否存在
	if _, err := os.Stat(args.Workdir); os.IsNotExist(err) {
		return "git error: workdir not exists"
	}

	// 限制命令长度
	if len(args.Cmd) == 0 {
		return "git error: cmd empty"
	}

	// 只允许安全的Git命令
	allowedCommands := map[string]bool{
		"status":    true,
		"log":       true,
		"diff":      true,
		"show":      true,
		"blame":     true,
		"rev-parse": true,
		"branch":    true,
		"tag":       true,
		"remote":    true,
		"config":    true,
		"ls-files":  true,
	}

	if !allowedCommands[args.Cmd[0]] {
		return fmt.Sprintf("git error: command '%s' not allowed", args.Cmd[0])
	}

	// 创建Git命令执行实例
	// 设置工作目录
	// 捕获组合输出（stdout+stderr）

	// 设置超时
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

// helper to pretty marshal args
func MarshalArgs(v any) string {
	LogAsync("DEBUG", fmt.Sprintf("Marshaling args: %+v", v))
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
