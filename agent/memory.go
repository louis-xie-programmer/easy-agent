// agent 包中的内存管理模块，提供：
// - 基于文件的持久化存储
// - 线程安全的读写操作
// - 对话历史和笔记的管理
// - 会话主题管理
// 使用JSON格式序列化数据
package agent

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ConversationSession 表示一个会话主题
type ConversationSession struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	Messages     []ChatMessage `json:"messages"`
}

// Memory 结构体实现会话记忆功能
// mu: 互斥锁，保证并发安全
// Conversations: 存储用户的所有对话提示
// Notes: 存储AI生成的回复笔记
// filepath: 持久化文件路径
type Memory struct {
	mu            sync.RWMutex
	Conversations []string `json:"conversations"`
	Notes         []string `json:"notes"`
	Sessions      map[string]*ConversationSession `json:"sessions"` // 新增会话管理
	CurrentSessionID string `json:"current_session_id"` // 当前会话ID
	filepath      string

	// 批量写入缓冲
	bufferMutex sync.Mutex
	buffer      []func()

	// 定时刷新
	dirty int32 // 使用原子操作替代布尔值
}

// NewFileMemory 创建基于文件的内存实例
// 参数：path 内存文件的存储路径
// 功能：
//  1. 尝试从文件加载现有数据
//  2. 如果文件不存在则创建新的Memory实例
//  3. 返回初始化的Memory指针
//
// 实现简单的持久化机制
func NewFileMemory(path string) (*Memory, error) {
	m := &Memory{
		Sessions: make(map[string]*ConversationSession),
		filepath: path,
	}
	if _, err := os.Stat(path); err == nil {
		bs, err := os.ReadFile(path)
		if err == nil {
			_ = json.Unmarshal(bs, m)
		}
	}
	// 启动定时持久化协程
	go func() {
		ticker := time.NewTicker(10 * time.Second) // 增加间隔时间
		defer ticker.Stop()
		for range ticker.C {
			m.flushBuffer()
		}
	}()
	return m, nil
}

// AddConversation 添加新的对话记录
// 线程安全：使用互斥锁保护共享资源
// 操作完成后立即持久化到文件
// 参数：s 用户输入的对话内容
func (m *Memory) AddConversation(s string) {
	m.executeInBatch(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.Conversations = append(m.Conversations, s)
	})
}

// AddNote 添加新的笔记记录
// 线程安全：使用互斥锁保护共享资源
// 操作完成后立即持久化到文件
// 参数：s AI生成的回复内容
func (m *Memory) AddNote(s string) {
	m.executeInBatch(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.Notes = append(m.Notes, s)
	})
}

// CreateSession 创建一个新的会话
func (m *Memory) CreateSession(sessionID, title string) {
	m.executeInBatch(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		
		session := &ConversationSession{
			ID:           sessionID,
			Title:        title,
			CreatedAt:    time.Now(),
			LastActiveAt: time.Now(),
			Messages:     make([]ChatMessage, 0),
		}
		
		m.Sessions[sessionID] = session
		m.CurrentSessionID = sessionID
	})
}

// SetCurrentSession 设置当前会话
func (m *Memory) SetCurrentSession(sessionID string) bool {
	m.mu.RLock()
	_, exists := m.Sessions[sessionID]
	m.mu.RUnlock()
	
	if !exists {
		return false
	}
	
	m.executeInBatch(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.CurrentSessionID = sessionID
		if session, ok := m.Sessions[sessionID]; ok {
			session.LastActiveAt = time.Now()
		}
	})
	
	return true
}

// AddMessageToSession 向指定会话添加消息
func (m *Memory) AddMessageToSession(sessionID string, message ChatMessage) bool {
	m.mu.RLock()
	session, exists := m.Sessions[sessionID]
	m.mu.RUnlock()
	
	if !exists {
		return false
	}
	
	m.executeInBatch(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		session.Messages = append(session.Messages, message)
		session.LastActiveAt = time.Now()
	})
	
	return true
}

// GetSessionMessages 获取会话消息历史
func (m *Memory) GetSessionMessages(sessionID string) ([]ChatMessage, bool) {
	m.mu.RLock()
	session, exists := m.Sessions[sessionID]
	m.mu.RUnlock()
	
	if !exists {
		return nil, false
	}
	
	// 返回副本以防止外部修改
	m.mu.RLock()
	defer m.mu.RUnlock()
	messages := make([]ChatMessage, len(session.Messages))
	copy(messages, session.Messages)
	return messages, true
}

// GetCurrentSessionID 获取当前会话ID
func (m *Memory) GetCurrentSessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.CurrentSessionID
}

// GetAllSessions 获取所有会话摘要信息
func (m *Memory) GetAllSessions() map[string]map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	sessionsInfo := make(map[string]map[string]interface{})
	for id, session := range m.Sessions {
		sessionsInfo[id] = map[string]interface{}{
			"title": session.Title,
			"created_at": session.CreatedAt,
			"last_active_at": session.LastActiveAt,
			"message_count": len(session.Messages),
		}
	}
	return sessionsInfo
}

// GetConversations 获取所有对话记录
func (m *Memory) GetConversations() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 返回副本以防止外部修改
	convs := make([]string, len(m.Conversations))
	copy(convs, m.Conversations)
	return convs
}

// GetNotes 获取所有笔记记录
func (m *Memory) GetNotes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 返回副本以防止外部修改
	notes := make([]string, len(m.Notes))
	copy(notes, m.Notes)
	return notes
}

// persist 将内存数据持久化到文件
// 使用JSON格式保存，带缩进便于阅读
// 错误处理被忽略（仅用于日志）
// 文件权限设置为0644
// 返回值：写入操作的错误信息（当前未返回）
func (m *Memory) persist() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 创建副本以减少锁定时间
	memCopy := &Memory{
		Conversations: make([]string, len(m.Conversations)),
		Notes:         make([]string, len(m.Notes)),
		Sessions:      make(map[string]*ConversationSession),
		CurrentSessionID: m.CurrentSessionID,
		filepath:      m.filepath,
	}
	copy(memCopy.Conversations, m.Conversations)
	copy(memCopy.Notes, m.Notes)
	
	// 复制会话数据
	for id, session := range m.Sessions {
		sessionCopy := &ConversationSession{
			ID:           session.ID,
			Title:        session.Title,
			CreatedAt:    session.CreatedAt,
			LastActiveAt: session.LastActiveAt,
			Messages:     make([]ChatMessage, len(session.Messages)),
		}
		copy(sessionCopy.Messages, session.Messages)
		memCopy.Sessions[id] = sessionCopy
	}

	bs, _ := json.MarshalIndent(memCopy, "", "  ")
	return os.WriteFile(m.filepath, bs, 0644)
}