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
	DefaultFlushInterval      = 5 * time.Second
	DefaultBatchSize          = 50
	DefaultSessionDirName     = "sessions"
	DefaultMemoryFileName     = "memory.json"
	DefaultSessionLoadLimit   = 200 // 启动时每个会话只加载最近 N 条消息到内存（节省内存）
	DefaultWriteQueueCapacity = 1000
)

// ---------- 持久化数据结构：MemoryStore（可序列化） ----------
type MemoryStorePersist struct {
	Conversations    []string                                  `json:"conversations"`
	Notes            []string                                  `json:"notes"`
	SessionsMeta     map[string]ConversationSessionMeta        `json:"sessions_meta"`
	CurrentSessionID string                                    `json:"current_session_id"`
}

type ConversationSessionMeta struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	MessageCount int       `json:"message_count"`
}

// ---------- 运行时内存结构 ----------
type MemoryV3 struct {
	// runtime protection
	mu sync.RWMutex

	// in-memory data
	conversations    []string
	notes            []string
	sessions         map[string]*ConversationSession
	currentSessionID string

	// persistence paths
	baseDir    string
	memoryPath string
	sessionDir string

	// writer queue and background goroutine
	writeQueue    chan func() error
	flushInterval time.Duration
	batchSize     int
	durableSync   bool

	// flags
	dirty    int32
	flushing int32

	// startup config
	sessionLoadLimit int
	closed           chan struct{}
}

// ConversationSession runtime struct (messages may be partial)
type ConversationSession struct {
	Meta     ConversationSessionMeta `json:"meta"`
	Messages []ChatMessage           `json:"messages"`
}

// ---------- Constructor / Loader ----------
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

	// apply options
	for _, o := range opts {
		o(mem)
	}

	// ensure directories
	if err := os.MkdirAll(mem.sessionDir, 0o755); err != nil {
		return nil, err
	}

	// load persisted state (non-fatal)
	if err := mem.loadFromDisk(); err != nil {
		fmt.Printf("[MemoryV3] loadFromDisk warning: %v\n", err)
	}

	// start background writer
	go mem.writerLoop()

	return mem, nil
}

// ---------- Options ----------
type MemoryV3Option func(*MemoryV3)

func WithFlushInterval(d time.Duration) MemoryV3Option {
	return func(m *MemoryV3) { m.flushInterval = d }
}
func WithBatchSize(sz int) MemoryV3Option {
	return func(m *MemoryV3) { m.batchSize = sz }
}
func WithDurableSync(enabled bool) MemoryV3Option {
	return func(m *MemoryV3) { m.durableSync = enabled }
}
func WithSessionLoadLimit(limit int) MemoryV3Option {
	return func(m *MemoryV3) { m.sessionLoadLimit = limit }
}

// ---------- Disk loading ----------
func (m *MemoryV3) loadFromDisk() error {
	// load memory.json if exists
	if _, err := os.Stat(m.memoryPath); err == nil {
		bs, err := os.ReadFile(m.memoryPath)
		if err != nil {
			return err
		}
		var store MemoryStorePersist
		if err := json.Unmarshal(bs, &store); err != nil {
			return err
		}
		// load into runtime
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

	// load session messages (jsonl) but limit how many we keep in memory per session
	fis, err := os.ReadDir(m.sessionDir)
	if err != nil {
		return nil // nothing to load
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

func ConversationSessionMetaToMeta(meta ConversationSessionMeta) ConversationSessionMeta {
	return ConversationSessionMeta{
		ID:           meta.ID,
		Title:        meta.Title,
		CreatedAt:    meta.CreatedAt,
		LastActiveAt: meta.LastActiveAt,
		MessageCount: meta.MessageCount,
	}
}

// ---------- Public API (threadsafe) ----------
func (m *MemoryV3) Close() error {
	// signal writerLoop to finish
	close(m.closed)
	// wait briefly then persist
	time.Sleep(100 * time.Millisecond)
	if err := m.persistStore(); err != nil {
		return err
	}
	return nil
}

func (m *MemoryV3) AddConversation(text string) {
	m.enqueueWrite(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.conversations = append(m.conversations, text)
		atomic.StoreInt32(&m.dirty, 1)
		return nil
	})
}

func (m *MemoryV3) AddNote(text string) {
	m.enqueueWrite(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.notes = append(m.notes, text)
		atomic.StoreInt32(&m.dirty, 1)
		return nil
	})
}

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

		// persist one message line to sessions/<id>.jsonl
		return m.appendSessionLine(sessionID, msg)
	})
	return true
}

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

func (m *MemoryV3) GetCurrentSessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentSessionID
}

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

func (m *MemoryV3) GetConversations() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.conversations))
	copy(out, m.conversations)
	return out
}
func (m *MemoryV3) GetNotes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.notes))
	copy(out, m.notes)
	return out
}

// ---------- Persistence helpers ----------

func (m *MemoryV3) enqueueWrite(task func() error) {
	select {
	case m.writeQueue <- task:
		// queued
	default:
		// queue full: execute non-blocking fallback to avoid blocking caller
		go func() { _ = task() }()
	}
	atomic.StoreInt32(&m.dirty, 1)
}

func (m *MemoryV3) writerLoop() {
	ticker := time.NewTicker(m.flushInterval)
	defer ticker.Stop()

	bufferTasks := make([]func() error, 0, m.batchSize)

	for {
		select {
		case <-m.closed:
			// drain remaining tasks
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
			// drain up to batchSize
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

func (m *MemoryV3) persistStore() error {
	// snapshot
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
