package agent

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// ConfirmationManager 管理待处理的工具执行确认请求。
// 它维护一个映射，将确认请求 ID 映射到用于传递用户响应的通道。
type ConfirmationManager struct {
	mu       sync.Mutex           // 互斥锁，用于保护 requests 映射的并发访问
	requests map[string]chan bool // 存储确认请求 ID 到结果通道的映射
}

// NewConfirmationManager 创建并返回一个新的 ConfirmationManager 实例。
func NewConfirmationManager() *ConfirmationManager {
	return &ConfirmationManager{
		requests: make(map[string]chan bool), // 初始化请求映射
	}
}

// RegisterRequest 注册一个新的确认请求。
// 它生成一个唯一的确认 ID，创建一个用于接收用户响应的通道，并将其存储在内部映射中。
// 同时，它会启动一个定时器，在一定时间后自动清理过期的请求，防止通道泄露。
// 返回生成的确认 ID 和用于接收用户响应的通道。
func (cm *ConfirmationManager) RegisterRequest() (string, chan bool) {
	cm.mu.Lock() // 获取锁，确保并发安全
	defer cm.mu.Unlock()

	id := uuid.New().String() // 生成唯一的确认 ID
	ch := make(chan bool, 1)  // 创建一个带缓冲的通道，用于传递布尔结果 (true 表示允许，false 表示拒绝)
	cm.requests[id] = ch      // 将请求 ID 和通道存储起来

	// 启动一个 goroutine，在 5 分钟后自动清理此请求，防止悬挂请求
	go func() {
		time.Sleep(5 * time.Minute) // 等待 5 分钟
		cm.mu.Lock()                // 获取锁以修改 requests 映射
		defer cm.mu.Unlock()
		if _, ok := cm.requests[id]; ok { // 再次检查请求是否存在，可能已被 ResolveRequest 处理
			close(ch)               // 关闭通道
			delete(cm.requests, id) // 从映射中删除请求
			Logger.Warn().Str("confirmation_id", id).Msg("Confirmation request timed out and was cleaned up.")
		}
	}()

	return id, ch
}

// ResolveRequest 解决一个确认请求。
// 它根据确认 ID 查找对应的通道，并将用户响应（允许或拒绝）发送到该通道。
// id: 要解决的确认请求的 ID。
// allowed: 用户是否允许执行操作 (true 表示允许，false 表示拒绝)。
func (cm *ConfirmationManager) ResolveRequest(id string, allowed bool) {
	cm.mu.Lock() // 获取锁，确保并发安全
	defer cm.mu.Unlock()

	if ch, ok := cm.requests[id]; ok { // 如果找到了对应的请求通道
		ch <- allowed           // 将用户响应发送到通道
		close(ch)               // 关闭通道
		delete(cm.requests, id) // 从映射中删除请求
		Logger.Info().Str("confirmation_id", id).Bool("allowed", allowed).Msg("Confirmation request resolved.")
	} else {
		Logger.Warn().Str("confirmation_id", id).Msg("Attempted to resolve a non-existent or already resolved confirmation request.")
	}
}
