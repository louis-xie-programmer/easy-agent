package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Logger 是全局的 zerolog 日志实例
var Logger zerolog.Logger

// logOnce 确保日志系统只被初始化一次
var logOnce sync.Once

// InitLogger 初始化全局日志系统
// 它配置了日志轮转、多重写入（文件和控制台）以及基于配置的日志级别过滤
func InitLogger(cfg Config) {
	logOnce.Do(func() {
		// 配置 lumberjack 用于日志轮转 (JSON 格式输出到文件)
		fileLogger := &lumberjack.Logger{
			Filename:   filepath.Join("logs", "app.log"), // 日志文件路径
			MaxSize:    10,                               // 每个日志文件的最大尺寸 (MB)
			MaxBackups: 5,                                // 保留的旧日志文件数量
			MaxAge:     30,                               // 日志文件保留天数
			Compress:   true,                             // 是否压缩旧的日志文件
		}

		// 配置控制台写入器 (人类可读格式)
		consoleWriter := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}

		// 创建一个多重写入器
		// 所有级别的日志都会以 JSON 格式写入文件
		// 只有达到或超过配置级别的日志才会写入控制台
		multiWriter := io.MultiWriter(fileLogger, consoleWriter)

		// 从配置中解析日志级别
		logLevel, err := zerolog.ParseLevel(strings.ToLower(cfg.Log.Level))
		if err != nil {
			logLevel = zerolog.InfoLevel // 如果解析失败，默认为 Info 级别
		}

		// 创建日志实例
		// Level() 方法设置了写入 multiWriter 的最低日志级别
		Logger = zerolog.New(multiWriter).
			Level(logLevel).
			With().
			Timestamp(). // 添加时间戳到每条日志
			Logger()

		// 备选方案：如果需要为文件和控制台设置不同的日志级别，可以使用 MultiLevelWriter
		// fileWriterWithLevel := zerolog.New(fileLogger).With().Timestamp().Logger() // 文件记录所有级别
		// consoleWriterWithLevel := zerolog.New(consoleWriter).Level(logLevel).With().Timestamp().Logger() // 控制台只记录指定级别及以上
		// Logger = zerolog.New(zerolog.MultiLevelWriter(fileWriterWithLevel, consoleWriterWithLevel)).With().Logger()

		Logger.Info().Msg("Logger initialized")
	})
}

// CloseLogger 在应用程序关闭时调用，用于记录日志系统关闭的消息
// 对于 lumberjack，不需要显式关闭文件句柄，它会在程序退出时自动处理
// 这个函数目前主要用于记录一条明确的关闭日志
func CloseLogger() {
	Logger.Info().Msg("Logger shutting down.")
}
