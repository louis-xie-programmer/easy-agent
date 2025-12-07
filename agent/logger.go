package agent

import (
	"log"
	"os"
	"sync"
	"time"
)

type LogEntry struct {
	Level   string
	Message string
	Time    time.Time
}

var (
	logChannel = make(chan LogEntry, 1000) // 增加缓冲区大小
	logFile    *os.File
	mu         sync.Mutex
	loggerOnce sync.Once // 确保日志只初始化一次
)

// InitLogger 初始化日志系统
func InitLogger() {
	loggerOnce.Do(func() {
		if err := os.MkdirAll("logs", 0755); err != nil {
			log.Fatalf("无法创建logs目录: %v", err)
		}
		var err error
		logFile, err = os.OpenFile("logs/app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("无法打开日志文件: %v", err)
		}
		log.SetOutput(logFile)
		go logProcessor()
	})
}

// LogAsync 异步记录入口
func LogAsync(level, msg string) {
	// 非阻塞写入，如果通道满了就丢弃日志而不是阻塞
	select {
	case logChannel <- LogEntry{
		Level:   level,
		Message: msg,
		Time:    time.Now(),
	}:
	default:
		// 丢弃日志以防止阻塞主程序
	}
}

// logProcessor 日志处理器协程
func logProcessor() {
	var logs []LogEntry
	ticker := time.NewTicker(1 * time.Second) // 减少刷盘频率
	defer ticker.Stop()

	for {
		select {
		case logEntry := <-logChannel:
			logs = append(logs, logEntry)
			// 当积累到一定数量时批量写入
			if len(logs) >= 10 {
				flushLogs(logs)
				logs = nil
			}
		case <-ticker.C:
			if len(logs) > 0 {
				flushLogs(logs)
				logs = nil
			}
		}
	}
}

// flushLogs 批量写入日志文件
func flushLogs(entries []LogEntry) {
	mu.Lock()
	defer mu.Unlock()

	for _, entry := range entries {
		logLine := formatLogLine(entry)
		if _, err := logFile.WriteString(logLine); err != nil {
			log.Printf("[LOGGER] 写入日志失败: %v", err)
		}
	}
	// 定期同步，而不是每次写入都同步
	logFile.Sync()
}

func formatLogLine(e LogEntry) string {
	return e.Time.Format("2006-01-02 15:04:05") +
		" [" + e.Level + "] " +
		e.Message + "\n"
}