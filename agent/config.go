package agent

import (
	"fmt"
	"log"
	"strings"

	"github.com/spf13/viper"
)

// Config 定义了应用程序的所有配置结构
// 使用 mapstructure 标签将配置文件中的键映射到结构体字段
type Config struct {
	// Service 服务相关配置
	Service struct {
		Version string `mapstructure:"version"` // 服务版本号
	} `mapstructure:"service"`
	// Server HTTP 服务器配置
	Server struct {
		Address    string `mapstructure:"address"`     // 监听地址，例如 ":8080"
		StaticPath string `mapstructure:"static_path"` // 静态文件目录路径
	} `mapstructure:"server"`
	// Ollama 大语言模型服务配置
	Ollama struct {
		URL          string   `mapstructure:"url"`           // Ollama API 地址
		DefaultModel string   `mapstructure:"default_model"` // 默认使用的模型名称
		Models       []string `mapstructure:"models"`        // 可用模型列表
		TimeoutSecs  int      `mapstructure:"timeout_secs"`  // 请求超时时间（秒）
	} `mapstructure:"ollama"`
	// Log 日志配置
	Log struct {
		Level string `mapstructure:"level"` // 日志级别 (debug, info, warn, error)
	} `mapstructure:"log"`
	// Storage 存储配置
	Storage struct {
		MemoryPath string `mapstructure:"memory_path"` // 会话记忆存储路径
		VectorPath string `mapstructure:"vector_path"` // 向量数据库存储路径
	} `mapstructure:"storage"`
	// Agent 代理核心配置
	Agent struct {
		MaxIterations int `mapstructure:"max_iterations"` // 最大思考/执行循环次数
	} `mapstructure:"agent"`
	// Embedding 向量嵌入配置
	Embedding struct {
		Model   string `mapstructure:"model"`    // 用于生成嵌入的模型名称
		APIPath string `mapstructure:"api_path"` // 嵌入 API 的路径
	} `mapstructure:"embedding"`
	// Sandbox 代码沙箱配置
	Sandbox struct {
		MaxConcurrency int     `mapstructure:"max_concurrency"` // 最大并发执行数
		DefaultTimeout int     `mapstructure:"default_timeout"` // 默认执行超时（秒）
		MaxTimeout     int     `mapstructure:"max_timeout"`     // 最大允许超时（秒）
		MemoryMB       int     `mapstructure:"memory_mb"`       // 内存限制 (MB)
		CpuQuota       float64 `mapstructure:"cpu_quota"`       // CPU 配额 (核心数)
	} `mapstructure:"sandbox"`
	// ToolValidation 工具调用验证配置
	ToolValidation struct {
		Keywords map[string][]string `mapstructure:"keywords"` // 每个工具对应的验证关键词列表
	} `mapstructure:"tool_validation"`
}

// LoadConfig 从配置文件、环境变量和默认值加载配置
// 返回加载后的 Config 结构体，如果出错则返回 error
func LoadConfig() (Config, error) {
	var cfg Config
	viper.SetConfigName("config") // 配置文件名 (不带扩展名)
	viper.SetConfigType("yaml")   // 配置文件类型
	viper.AddConfigPath(".")      // 在当前目录查找配置文件

	// --- 设置默认值 ---
	// Service
	viper.SetDefault("service.version", "v0.1.0")
	// Server
	viper.SetDefault("server.address", ":8080")
	viper.SetDefault("server.static_path", "./client")
	// Ollama
	viper.SetDefault("ollama.url", "http://localhost:11434/api/chat")
	viper.SetDefault("ollama.default_model", "qwen2.5-coder:3b")
	viper.SetDefault("ollama.timeout_secs", 300) // 5 minutes
	// Log
	viper.SetDefault("log.level", "INFO")
	// Storage
	viper.SetDefault("storage.memory_path", "./memory_store")
	viper.SetDefault("storage.vector_path", "./memory_store")
	// Agent
	viper.SetDefault("agent.max_iterations", 6)
	// Embedding
	viper.SetDefault("embedding.model", "nomic-embed-text")
	viper.SetDefault("embedding.api_path", "/api/embeddings")
	// Sandbox
	viper.SetDefault("sandbox.max_concurrency", 5)
	viper.SetDefault("sandbox.default_timeout", 60) // 60 seconds
	viper.SetDefault("sandbox.max_timeout", 300)    // 300 seconds
	viper.SetDefault("sandbox.memory_mb", 256)
	viper.SetDefault("sandbox.cpu_quota", 0.5)

	// ToolValidation Defaults
	// 设置工具验证的默认关键词，支持多语言
	viper.SetDefault("tool_validation.keywords.read_file", []string{"file", "read", "write", "save", "open", "path", "tệp", "đọc", "ghi", "lưu", "mở", "đường dẫn", "文件", "读取", "写入", "保存", "路径", "打开"})
	viper.SetDefault("tool_validation.keywords.write_file", []string{"file", "read", "write", "save", "open", "path", "tệp", "đọc", "ghi", "lưu", "mở", "đường dẫn", "文件", "读取", "写入", "保存", "路径", "打开"})
	viper.SetDefault("tool_validation.keywords.run_code", []string{"run", "execute", "code", "script", "chạy", "thực thi", "mã", "运行", "执行", "代码", "开发", "写", "编写", "implement", "develop", "write"})
	// 移除了通用的词汇如 "create", "new", "创建", "新建" 以防止误报
	viper.SetDefault("tool_validation.keywords.create_session", []string{"session", "conversation", "chat", "topic", "switch", "hội thoại", "chủ đề", "trò chuyện", "chuyển", "会话", "聊天", "主题", "切换"})
	viper.SetDefault("tool_validation.keywords.switch_session", []string{"session", "conversation", "chat", "topic", "switch", "hội thoại", "chủ đề", "trò chuyện", "chuyển", "会话", "聊天", "主题", "切换"})
	viper.SetDefault("tool_validation.keywords.web_search", []string{"search", "find", "what is", "how to", "who is", "tell me about", "tìm", "là gì", "hướng dẫn", "ai là", "kể cho tôi về", "搜索", "查找", "是什么", "如何", "谁是", "告诉我关于"})
	viper.SetDefault("tool_validation.keywords.knowledge_search", []string{"search", "find", "what is", "how to", "who is", "tell me about", "tìm", "là gì", "hướng dẫn", "ai là", "kể cho tôi về", "搜索", "查找", "是什么", "如何", "谁是", "告诉我关于"})

	// 从环境变量读取配置
	viper.AutomaticEnv()
	viper.SetEnvPrefix("EASYAGENT") // 环境变量前缀，例如 EASYAGENT_SERVER_ADDRESS
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// 配置文件未找到，可以忽略，使用默认值和环境变量
			// 此时日志系统尚未初始化，使用标准日志包
			log.Println("[WARN] Config file not found, using defaults and environment variables.")
		} else {
			// 配置文件找到但解析错误
			return cfg, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	// 将配置解析到结构体
	if err := viper.Unmarshal(&cfg); err != nil {
		return cfg, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return cfg, nil
}
