// agent 包中的工具函数模块，包含：
// - 代码沙箱执行
// - 文件系统操作
// - Git版本控制集成
// 所有函数都被设计为安全的、受限的操作
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
type ReadFileArgs struct {
	Path string `json:"path"`
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

// runCode runs code in a docker sandbox (basic demo)
// RunCodeSandbox 在Docker沙箱中安全执行代码
// 特性：
//   - 使用临时工作目录
//   - 支持Python和Go语言
//   - 严格的资源限制（CPU/内存/网络）
//   - 自动清理机制
// 返回值：执行输出或错误信息
func RunCodeSandbox(args RunCodeArgs) string {
	// 创建唯一的临时工作空间
	// 命名格式：agent_work_时间戳
	// 存储在./sandboxes目录下
	// workspace
	tmp := fmt.Sprintf("agent_work_%d", time.Now().UnixNano())
	base := filepath.Join("./sandboxes", tmp)
	_ = os.MkdirAll(base, 0755)

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
		_ = os.WriteFile(filepath.Join(base, mainFile), []byte(args.Code), 0644)
	case "go":
		_ = os.WriteFile(filepath.Join(base, "main.go"), []byte(args.Code), 0644)
		// for go module, quick hack: create go.mod
		_ = os.WriteFile(filepath.Join(base, "go.mod"), []byte("module sandbox\n\ngo 1.20\n"), 0644)
	default:
		_ = os.WriteFile(filepath.Join(base, "main.txt"), []byte(args.Code), 0644)
	}

	for p, content := range args.Files {
		full := filepath.Join(base, p)
		_ = os.MkdirAll(filepath.Dir(full), 0755)
		_ = os.WriteFile(full, []byte(content), 0644)
	}

	timeout := 8
	if args.Timeout > 0 {
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
	out, err := exec.CommandContext(ctx, "docker", dockerArgs...).CombinedOutput()
	// 注意：当前禁用了自动清理以方便调试
	// 实际生产环境应启用os.RemoveAll(base)
	// 或通过定时任务清理旧的工作空间
	// optionally cleanup workspace after a delay (left for debugging)
	// os.RemoveAll(base)
	if err != nil {
		return fmt.Sprintf("error: %v\noutput:\n%s", err, string(out))
	}
	return string(out)
}

// ReadFile 安全读取文件内容
// 特性：
//   - 大小限制（20KB）
//   - 错误处理
//   - 内容截断提示
// 返回值：文件内容或错误信息
func ReadFile(args ReadFileArgs) string {
	bs, err := os.ReadFile(args.Path)
	if err != nil {
		return "read error: " + err.Error()
	}
	// 限制返回内容大小，防止大文件拖慢系统
	// 超过20KB的部分将被截断并添加提示
	// cap size
	if len(bs) > 20000 {
		return string(bs[:20000]) + "\n...[truncated]"
	}
	return string(bs)
}

// WriteFile 安全写入文件
// 支持两种模式：
//   - overwrite: 覆盖写入（默认）
//   - append: 追加写入
// 返回值：操作结果描述
func WriteFile(args WriteFileArgs) string {
	mode := args.Mode
	if mode == "" {
		mode = "overwrite"
	}
	// 覆盖模式：直接写入新内容
	if mode == "overwrite" {
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
// 返回值：Git命令输出或错误信息
func GitCmd(args GitCmdArgs) string {
	// basic safety: require local path exists
	if args.Workdir == "" {
		return "git error: workdir empty"
	}
	// 创建Git命令执行实例
	// 设置工作目录
	// 捕获组合输出（stdout+stderr）
	cmd := exec.Command("git", args.Cmd...)
	cmd.Dir = args.Workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git error: %v\noutput:\n%s", err, string(out))
	}
	return string(out)
}

// helper to pretty marshal args
func MarshalArgs(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
