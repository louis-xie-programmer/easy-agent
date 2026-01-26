package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Document 代表一条知识，包含其向量嵌入。
type Document struct {
	ID        string         `json:"id"`        // 文档的唯一标识符
	Content   string         `json:"content"`   // 文档的文本内容
	Metadata  map[string]any `json:"metadata"`  // 文档的元数据，例如来源、块索引等
	Embedding []float64      `json:"embedding"` // 文档内容的向量嵌入
}

// SearchResult 代表向量存储中的单个搜索结果。
type SearchResult struct {
	Doc   Document // 匹配的文档
	Score float64  // 查询向量与文档向量的相似度得分
}

// VectorStore 是任何向量数据库的接口。
// 这允许多种实现（例如，内存、Chroma、Pinecone 等）。
type VectorStore interface {
	// Add 将一个文档添加到存储中。
	Add(doc Document) error
	// Search 根据查询向量在存储中搜索最相似的文档。
	// topK: 返回最相似结果的数量。
	Search(queryVec []float64, topK int) ([]SearchResult, error)
	// Close 关闭向量存储，释放资源。
	Close() error
}

// --- 内存向量存储实现 ---

// InMemoryVectorStore 是一个简单的内存向量存储实现。
// 它适用于开发和小型应用程序。
type InMemoryVectorStore struct {
	docs     []Document   // 存储在内存中的文档列表
	mu       sync.RWMutex // 读写互斥锁，用于保护 docs 的并发访问
	filePath string       // JSONL 文件的路径，用于持久化

	// 异步持久化
	writeQueue chan Document  // 写入队列，用于异步持久化文档
	wg         sync.WaitGroup // 等待组，用于等待后台写入完成
	closed     chan struct{}  // 关闭信号通道
}

// NewInMemoryVectorStore 创建一个新的内存向量存储。
// persistDir: 持久化目录的路径。如果为空，则不进行持久化。
func NewInMemoryVectorStore(persistDir string) (*InMemoryVectorStore, error) {
	vs := &InMemoryVectorStore{
		docs:       make([]Document, 0),
		writeQueue: make(chan Document, 1000), // 带缓冲的通道，用于异步写入
		closed:     make(chan struct{}),
	}

	if persistDir != "" {
		if err := os.MkdirAll(persistDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create persist directory: %w", err)
		}
		vs.filePath = filepath.Join(persistDir, "vectors.jsonl") // 使用 .jsonl 扩展名
		if err := vs.loadJSONL(); err != nil {
			// 记录错误，但不中断初始化
			Logger.Warn().Err(err).Msg("Failed to load vector store from disk")
		}
	}

	// 启动后台持久化 goroutine
	vs.wg.Add(1)
	go vs.persistenceLoop()

	return vs, nil
}

// Add 将一个文档添加到存储中，并将其排队等待持久化。
func (vs *InMemoryVectorStore) Add(doc Document) error {
	vs.mu.Lock()
	vs.docs = append(vs.docs, doc)
	vs.mu.Unlock()

	// 非阻塞地写入队列
	select {
	case vs.writeQueue <- doc:
		// 文档成功排队等待异步写入
	default:
		// 如果队列已满，则记录警告并丢弃该文档的异步写入
		Logger.Warn().Msg("VectorStore write queue is full, dropping document for async write.")
	}
	return nil
}

// Search 在存储中的文档上执行余弦相似度搜索。
// queryVec: 查询向量。
// topK: 返回最相似结果的数量。
func (vs *InMemoryVectorStore) Search(queryVec []float64, topK int) ([]SearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var results []SearchResult

	for _, doc := range vs.docs {
		if len(doc.Embedding) != len(queryVec) {
			continue // 跳过嵌入维度不匹配的文档
		}
		score := cosineSimilarity(queryVec, doc.Embedding)
		results = append(results, SearchResult{
			Doc:   doc,
			Score: score,
		})
	}

	// 按得分降序对结果进行排序
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > topK {
		return results[:topK], nil
	}
	return results, nil
}

// Close 优雅地关闭持久化循环。
func (vs *InMemoryVectorStore) Close() error {
	// 发出信号，通知 persistenceLoop 停止并处理所有剩余的项目
	close(vs.writeQueue)
	vs.wg.Wait() // 等待 persistenceLoop 完成
	return nil
}

// loadJSONL 从磁盘上的 JSONL 文件读取向量存储。
func (vs *InMemoryVectorStore) loadJSONL() error {
	if vs.filePath == "" {
		return nil
	}

	file, err := os.OpenFile(vs.filePath, os.O_RDONLY|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("failed to open vector store file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var loadedDocs []Document
	for scanner.Scan() {
		var doc Document
		if err := json.Unmarshal(scanner.Bytes(), &doc); err != nil {
			Logger.Warn().Err(err).Msg("Failed to unmarshal document from vector store file, skipping line.")
			continue
		}
		loadedDocs = append(loadedDocs, doc)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading vector store file: %w", err)
	}

	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.docs = loadedDocs
	Logger.Info().Int("count", len(loadedDocs)).Str("path", vs.filePath).Msg("Loaded documents from vector store")
	return nil
}

// appendDocumentToJSONL 将单个文档追加到 JSONL 文件。
func (vs *InMemoryVectorStore) appendDocumentToJSONL(doc Document) error {
	if vs.filePath == "" {
		return nil
	}

	file, err := os.OpenFile(vs.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open vector store file for append: %w", err)
	}
	defer file.Close()

	line, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to marshal document for append: %w", err)
	}

	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("failed to write document to file: %w", err)
	}
	return nil
}

// persistenceLoop 是将文档保存到磁盘的后台 goroutine。
func (vs *InMemoryVectorStore) persistenceLoop() {
	defer vs.wg.Done()

	for {
		select {
		case doc, ok := <-vs.writeQueue:
			if !ok { // 通道已关闭
				return // 退出 goroutine
			}
			if err := vs.appendDocumentToJSONL(doc); err != nil {
				Logger.Error().Err(err).Msg("Failed to persist document to vector store.")
			}
		case <-vs.closed: // 此通道不再使用，writeQueue 的关闭处理了关闭逻辑
			return
		}
	}
}

// cosineSimilarity 计算两个向量之间的余弦相似度。
func cosineSimilarity(a, b []float64) float64 {
	var dotProduct, normA, normB float64
	for i := 0; i < len(a); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}
