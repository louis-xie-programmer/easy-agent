// agent/memory_v3.go
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 可配置常量 ----------
const (
	DefaultFlushInterval      = 5 * time.Second // 默认刷新间隔
	DefaultBatchSize          = 50              // 默认批处理大小
	DefaultSessionDirName     = "sessions"      // 默认会话目录名称
	DefaultMemoryFileName     = "memory.json"   // 默认内存文件名
	DefaultSessionLoadLimit   = 200             // 启动时每个会话只加载最近 N 条消息到内存（节省内存）
	DefaultWriteQueueCapacity = 1000            // 默认写入队列容量
)

// ---------- 持久化数据结构：MemoryStore（可序列化） ----------
// MemoryStorePersist 是用于持久化到 memory.json 的数据结构
type MemoryStorePersist struct {
	Conversations    []string                           `json:"conversations"`      // 对话列表
	Notes            []string                           `json:"notes"`              // 笔记列表
	SessionsMeta     map[string]ConversationSessionMeta `json:"sessions_meta"`      // 会话元数据映射
	CurrentSessionID string                             `json:"current_session_id"` // 当前会话 ID
}

// ConversationSessionMeta 是会话的元数据结构
type ConversationSessionMeta struct {
	ID           string    `json:"id"`             // 会话 ID
	Title        string    `json:"title"`          // 会话标题
	CreatedAt    time.Time `json:"created_at"`     // 创建时间
	LastActiveAt time.Time `json:"last_active_at"` // 最后活动时间
	MessageCount int       `json:"message_count"`  // 消息数量
}

// ---------- 运行时内存结构 ----------
// MemoryV3 是运行时的内存结构
type MemoryV3 struct {
	// 运行时保护
	mu sync.RWMutex

	// 内存中的数据
	conversations    []string
	notes            []string
	sessions         map[string]*ConversationSession
	currentSessionID string

	// 持久化路径
	baseDir    string
	memoryPath string
	sessionDir string

	// 写入队列和后台 goroutine
	writeQueue    chan func() error
	flushInterval time.Duration
	batchSize     int
	durableSync   bool
	wg            sync.WaitGroup // 用于等待后台写入完成

	// 标志
	dirty    int32
	flushing int32

	// 启动配置
	sessionLoadLimit int
	closed           chan struct{}
}

// ConversationSession 是运行时的会话结构（消息可能是部分的）
type ConversationSession struct {
	Meta     ConversationSessionMeta `json:"meta"`     // 会话元数据
	Messages []ChatMessage           `json:"messages"` // 会话消息
}

// ---------- 构造函数 / 加载器 ----------
// NewMemoryV3 创建一个新的 MemoryV3 实例
func NewMemoryV3(baseDir string, opts ...MemoryV3Option) (*MemoryV3, error) {
	if baseDir == "" {
		baseDir = "./memory_store"
	}
	mem := &MemoryV3{
		conversations:    make([]string, 0),
		notes:            make([]string, 0),
		sessions:         make(map[string]*ConversationSession),
		baseDir:          baseDir,
		memoryPath:       filepath.Join(baseDir, DefaultMemoryFileName),
		sessionDir:       filepath.Join(baseDir, DefaultSessionDirName),
		writeQueue:       make(chan func() error, DefaultWriteQueueCapacity),
		flushInterval:    DefaultFlushInterval,
		batchSize:        DefaultBatchSize,
		durableSync:      false,
		sessionLoadLimit: DefaultSessionLoadLimit,
		closed:           make(chan struct{}),
	}

	// 应用选项
	for _, o := range opts {
		o(mem)
	}

	// 确保目录存在
	if err := os.MkdirAll(mem.sessionDir, 0o755); err != nil {
		return nil, err
	}

	// 加载持久化状态（非致命）
	if err := mem.loadFromDisk(); err != nil {
		fmt.Printf("[MemoryV3] loadFromDisk warning: %v\n", err)
	}

	// 启动后台写入器
	mem.wg.Add(1)
	go mem.writerLoop()

	return mem, nil
}

// ---------- 选项 ----------
// MemoryV3Option 是 MemoryV3 的选项函数
type MemoryV3Option func(*MemoryV3)

// WithFlushInterval 设置刷新间隔
func WithFlushInterval(d time.Duration) MemoryV3Option {
	return func(m *MemoryV3) { m.flushInterval = d }
}

// WithBatchSize 设置批处理大小
func WithBatchSize(sz int) MemoryV3Option {
	return func(m *MemoryV3) { m.batchSize = sz }
}

// WithDurableSync 设置是否启用持久化同步
func WithDurableSync(enabled bool) MemoryV3Option {
	return func(m *MemoryV3) { m.durableSync = enabled }
}

// WithSessionLoadLimit 设置会话加载限制
func WithSessionLoadLimit(limit int) MemoryV3Option {
	return func(m *MemoryV3) { m.sessionLoadLimit = limit }
}

// ---------- 从磁盘加载 ----------
// loadFromDisk 从磁盘加载持久化状态
func (m *MemoryV3) loadFromDisk() error {
	// 如果存在，则加载 memory.json
	if _, err := os.Stat(m.memoryPath); err == nil {
		bs, err := os.ReadFile(m.memoryPath)
		if err != nil {
			return err
		}
		var store MemoryStorePersist
		if err := json.Unmarshal(bs, &store); err != nil {
			return err
		}
		// 加载到运行时
		m.mu.Lock()
		m.conversations = append([]string{}, store.Conversations...)
		m.notes = append([]string{}, store.Notes...)
		m.currentSessionID = store.CurrentSessionID
		for id, meta := range store.SessionsMeta {
			m.sessions[id] = &ConversationSession{
				Meta:     ConversationSessionMetaToMeta(meta),
				Messages: make([]ChatMessage, 0),
			}
		}
		m.mu.Unlock()
	}

	// 加载会话消息 (jsonl)，但限制每个会话在内存中保留的数量
	fis, err := os.ReadDir(m.sessionDir)
	if err != nil {
		return nil // 无需加载
	}
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		sessionFile := filepath.Join(m.sessionDir, fi.Name())
		sessionID := fi.Name()
		f, err := os.Open(sessionFile)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		msgs := make([]ChatMessage, 0)
		total := 0
		for scanner.Scan() {
			var msg ChatMessage
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				continue
			}
			total++
			msgs = append(msgs, msg)
			if len(msgs) > m.sessionLoadLimit {
				msgs = msgs[len(msgs)-m.sessionLoadLimit:]
			}
		}
		f.Close()
		if len(msgs) > 0 {
			m.mu.Lock()
			if session, ok := m.sessions[sessionID]; ok {
				session.Messages = msgs
				session.Meta.MessageCount = total
			} else {
				m.sessions[sessionID] = &ConversationSession{
					Meta: ConversationSessionMeta{
						ID:           sessionID,
						Title:        sessionID,
						CreatedAt:    time.Now(),
						LastActiveAt: time.Now(),
						MessageCount: total,
					},
					Messages: msgs,
				}
			}
			m.mu.Unlock()
		}
	}
	return nil
}

// ConversationSessionMetaToMeta 将 ConversationSessionMeta 转换为 ConversationSessionMeta
func ConversationSessionMetaToMeta(meta ConversationSessionMeta) ConversationSessionMeta {
	return ConversationSessionMeta{
		ID:           meta.ID,
		Title:        meta.Title,
		CreatedAt:    meta.CreatedAt,
		LastActiveAt: meta.LastActiveAt,
		MessageCount: meta.MessageCount,
	}
}

// ---------- 公共 API (线程安全) ----------
// Close 关闭 MemoryV3 实例
func (m *MemoryV3) Close() error {
	// 通知 writerLoop 完成
	close(m.closed)
	// 等待 writerLoop 完成
	m.wg.Wait()

	// 最终持久化
	if err := m.persistStore(); err != nil {
		return err
	}
	return nil
}

// AddConversation 添加对话
func (m *MemoryV3) AddConversation(text string) {
	m.enqueueWrite(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.conversations = append(m.conversations, text)
		atomic.StoreInt32(&m.dirty, 1)
		return nil
	})
}

// AddNote 添加笔记
func (m *MemoryV3) AddNote(text string) {
	m.enqueueWrite(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.notes = append(m.notes, text)
		atomic.StoreInt32(&m.dirty, 1)
		return nil
	})
}

// CreateSession 创建会话
func (m *MemoryV3) CreateSession(sessionID, title string) {
	m.enqueueWrite(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		now := time.Now()
		m.sessions[sessionID] = &ConversationSession{
			Meta: ConversationSessionMeta{
				ID:           sessionID,
				Title:        title,
				CreatedAt:    now,
				LastActiveAt: now,
				MessageCount: 0,
			},
			Messages: make([]ChatMessage, 0),
		}
		m.currentSessionID = sessionID
		atomic.StoreInt32(&m.dirty, 1)
		return nil
	})
}

// SetCurrentSession 设置当前会话
func (m *MemoryV3) SetCurrentSession(sessionID string) bool {
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	m.enqueueWrite(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.currentSessionID = sessionID
		if s, ok := m.sessions[sessionID]; ok {
			s.Meta.LastActiveAt = time.Now()
		}
		atomic.StoreInt32(&m.dirty, 1)
		return nil
	})
	return true
}

// AddMessageToSession 向会话添加消息
func (m *MemoryV3) AddMessageToSession(sessionID string, msg ChatMessage) bool {
	m.mu.RLock()
	session, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	m.enqueueWrite(func() error {
		m.mu.Lock()
		session.Messages = append(session.Messages, msg)
		session.Meta.LastActiveAt = time.Now()
		session.Meta.MessageCount++
		m.mu.Unlock()

		// 将一条消息行持久化到 sessions/<id>.jsonl
		return m.appendSessionLine(sessionID, msg)
	})
	return true
}

// GetSessionMessages 获取会话消息
func (m *MemoryV3) GetSessionMessages(sessionID string) ([]ChatMessage, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, false
	}
	out := make([]ChatMessage, len(s.Messages))
	copy(out, s.Messages)
	return out, true
}

// GetCurrentSessionID 获取当前会话 ID
func (m *MemoryV3) GetCurrentSessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentSessionID
}

// GetAllSessions 获取所有会话
func (m *MemoryV3) GetAllSessions() map[string]map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ret := make(map[string]map[string]interface{}, len(m.sessions))
	for id, s := range m.sessions {
		ret[id] = map[string]interface{}{
			"title":          s.Meta.Title,
			"created_at":     s.Meta.CreatedAt,
			"last_active_at": s.Meta.LastActiveAt,
			"message_count":  s.Meta.MessageCount,
		}
	}
	return ret
}

// GetConversations 获取所有对话
func (m *MemoryV3) GetConversations() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.conversations))
	copy(out, m.conversations)
	return out
}

// GetNotes 获取所有笔记
func (m *MemoryV3) GetNotes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.notes))
	copy(out, m.notes)
	return out
}

// ---------- 持久化帮助程序 ----------

// enqueueWrite 将写入任务排入队列
func (m *MemoryV3) enqueueWrite(task func() error) {
	select {
	case m.writeQueue <- task:
		// 已排队
	default:
		// 队列已满：执行非阻塞回退以避免阻塞调用者
		go func() { _ = task() }()
	}
	atomic.StoreInt32(&m.dirty, 1)
}

// writerLoop 是后台写入循环
func (m *MemoryV3) writerLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.flushInterval)
	defer ticker.Stop()

	bufferTasks := make([]func() error, 0, m.batchSize)

	for {
		select {
		case <-m.closed:
			// 耗尽剩余任务
			for {
				select {
				case t := <-m.writeQueue:
					bufferTasks = append(bufferTasks, t)
					if len(bufferTasks) >= m.batchSize {
						m.runTasks(bufferTasks)
						bufferTasks = bufferTasks[:0]
					}
				default:
					if len(bufferTasks) > 0 {
						m.runTasks(bufferTasks)
					}
					_ = m.persistStore()
					return
				}
			}

		case <-ticker.C:
			// 耗尽最多 batchSize 个任务
			n := 0
			for n < m.batchSize {
				select {
				case t := <-m.writeQueue:
					bufferTasks = append(bufferTasks, t)
					n++
				default:
					n = m.batchSize
				}
			}
			if len(bufferTasks) > 0 {
				m.runTasks(bufferTasks)
				bufferTasks = bufferTasks[:0]
			}
			if atomic.LoadInt32(&m.dirty) == 1 {
				_ = m.persistStore()
			}

		case t := <-m.writeQueue:
			bufferTasks = append(bufferTasks, t)
			if len(bufferTasks) >= m.batchSize {
				m.runTasks(bufferTasks)
				bufferTasks = bufferTasks[:0]
			}
		}
	}
}

// runTasks 运行任务
func (m *MemoryV3) runTasks(tasks []func() error) {
	if len(tasks) == 0 {
		return
	}
	if !atomic.CompareAndSwapInt32(&m.flushing, 0, 1) {
		for _, t := range tasks {
			select {
			case m.writeQueue <- t:
			default:
				_ = t()
			}
		}
		return
	}
	defer atomic.StoreInt32(&m.flushing, 0)

	for _, t := range tasks {
		_ = t()
	}
	if atomic.LoadInt32(&m.dirty) == 1 {
		_ = m.persistStore()
		atomic.StoreInt32(&m.dirty, 0)
	}
}

// persistStore 持久化存储
func (m *MemoryV3) persistStore() error {
	// 快照
	m.mu.RLock()
	store := MemoryStorePersist{
		Conversations:    append([]string{}, m.conversations...),
		Notes:            append([]string{}, m.notes...),
		SessionsMeta:     make(map[string]ConversationSessionMeta, len(m.sessions)),
		CurrentSessionID: m.currentSessionID,
	}
	for id, s := range m.sessions {
		store.SessionsMeta[id] = ConversationSessionMeta{
			ID:           s.Meta.ID,
			Title:        s.Meta.Title,
			CreatedAt:    s.Meta.CreatedAt,
			LastActiveAt: s.Meta.LastActiveAt,
			MessageCount: s.Meta.MessageCount,
		}
	}
	m.mu.RUnlock()

	tmpPath := m.memoryPath + ".tmp"
	bs, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, bs, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, m.memoryPath); err != nil {
		return err
	}
	if m.durableSync {
		dirF, _ := os.Open(m.baseDir)
		if dirF != nil {
			_ = dirF.Sync()
			_ = dirF.Close()
		}
	}
	return nil
}

// appendSessionLine 向会话文件追加一行
func (m *MemoryV3) appendSessionLine(sessionID string, msg ChatMessage) error {
	path := filepath.Join(m.sessionDir, sessionID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, byte('\n'))); err != nil {
		return err
	}
	if m.durableSync {
		_ = f.Sync()
	}
	return nil
}
