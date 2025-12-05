// agent 包中的内存管理模块，提供：
// - 基于文件的持久化存储
// - 线程安全的读写操作
// - 对话历史和笔记的管理
// 使用JSON格式序列化数据
package agent

import (
	"encoding/json"
	"os"
	"sync"
)

// Memory 结构体实现会话记忆功能
// mu: 互斥锁，保证并发安全
// Conversations: 存储用户的所有对话提示
// Notes: 存储AI生成的回复笔记
// filepath: 持久化文件路径
type Memory struct {
	mu            sync.Mutex
	Conversations []string `json:"conversations"`
	Notes         []string `json:"notes"`
	filepath      string
}

// NewFileMemory 创建基于文件的内存实例
// 参数：path 内存文件的存储路径
// 功能：
//   1. 尝试从文件加载现有数据
//   2. 如果文件不存在则创建新的Memory实例
//   3. 返回初始化的Memory指针
// 实现简单的持久化机制
func NewFileMemory(path string) (*Memory, error) {
	m := &Memory{filepath: path}
	if _, err := os.Stat(path); err == nil {
		bs, err := os.ReadFile(path)
		if err == nil {
			_ = json.Unmarshal(bs, m)
		}
	}
	return m, nil
}

// AddConversation 添加新的对话记录
// 线程安全：使用互斥锁保护共享资源
// 操作完成后立即持久化到文件
// 参数：s 用户输入的对话内容
func (m *Memory) AddConversation(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Conversations = append(m.Conversations, s)
	_ = m.persist()
}

// AddNote 添加新的笔记记录
// 线程安全：使用互斥锁保护共享资源
// 操作完成后立即持久化到文件
// 参数：s AI生成的回复内容
func (m *Memory) AddNote(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Notes = append(m.Notes, s)
	_ = m.persist()
}

// persist 将内存数据持久化到文件
// 使用JSON格式保存，带缩进便于阅读
// 错误处理被忽略（仅用于日志）
// 文件权限设置为0644
// 返回值：写入操作的错误信息（当前未返回）
func (m *Memory) persist() error {
	bs, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(m.filepath, bs, 0644)
}
