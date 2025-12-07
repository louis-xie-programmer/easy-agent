// memory_batch.go
// 批量写入和定时刷新功能实现
// 避免频繁I/O操作，提高系统性能

package agent

import (
	"sync/atomic"
)

// executeInBatch 将修改操作加入批量执行队列
// 使用缓冲通道控制并发，避免锁竞争
func (m *Memory) executeInBatch(op func()) {
	m.bufferMutex.Lock()
	m.buffer = append(m.buffer, op)
	atomic.StoreInt32(&m.dirty, 1) // 使用原子操作标记为脏数据
	m.bufferMutex.Unlock()
}

// flushBuffer 刷新批量缓冲区到磁盘
// 原子性处理所有待办操作，减少文件I/O次数
func (m *Memory) flushBuffer() {
	// 使用原子操作检查是否需要刷新
	if atomic.LoadInt32(&m.dirty) == 0 {
		return
	}
	
	m.bufferMutex.Lock()
	if len(m.buffer) == 0 || atomic.LoadInt32(&m.dirty) == 0 {
		m.bufferMutex.Unlock()
		return
	}
	
	ops := m.buffer
	m.buffer = nil
	atomic.StoreInt32(&m.dirty, 0) // 重置脏标记
	m.bufferMutex.Unlock()

	// 执行所有操作
	for _, op := range ops {
		op()
	}

	// 持久化到文件
	_ = m.persist()
}