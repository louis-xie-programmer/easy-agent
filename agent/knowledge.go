package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// IngestContent 处理文本内容：分割、嵌入，并将其存储在向量存储中
// 此版本使用工作池并发嵌入文本块，以提高性能
// source: 内容来源标识符
// content: 要处理的文本内容
func (a *Agent) IngestContent(source string, content string) error {
	ctx, span := tracer.Start(context.Background(), "Agent.IngestContent",
		trace.WithAttributes(
			attribute.String("source", source),
			attribute.Int("content.length", len(content)),
		),
	)
	defer span.End()

	// 1. 智能文本分割
	chunks := recursiveSplit(content, 500, 50) // 将文本分割成大小为 500 字符，重叠 50 字符的块
	span.SetAttributes(attribute.Int("chunks.count", len(chunks)))
	Logger.Info().Str("source", source).Int("chunk_count", len(chunks)).Msg("Ingesting content")

	// 2. 使用工作池并发嵌入
	const numWorkers = 8                         // 并发工作协程的数量
	jobs := make(chan int, len(chunks))          // 任务通道，用于分发 chunk 索引
	results := make(chan *Document, len(chunks)) // 结果通道，用于收集嵌入后的文档
	var wg sync.WaitGroup                        // 等待组，用于等待所有工作协程完成

	// 启动工作协程
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range jobs { // 从任务通道接收 chunk 索引
				chunk := chunks[i]
				chunkSpanCtx, chunkSpan := tracer.Start(ctx, "Agent.IngestContent.Chunk",
					trace.WithAttributes(
						attribute.String("chunk.source", source),
						attribute.Int("chunk.index", i),
						attribute.Int("chunk.length", len(chunk)),
						attribute.Int("worker.id", workerID),
					),
				)

				// 调用 LLM 嵌入文本块
				vec, err := a.llm.Embed(chunkSpanCtx, chunk)
				if err != nil {
					Logger.Error().Err(err).Int("chunk_index", i).Str("source", source).Msg("Embed failed for chunk")
					chunkSpan.RecordError(err)
					chunkSpan.SetStatus(codes.Error, fmt.Sprintf("Embed failed: %v", err))
					chunkSpan.End()
					results <- nil // 发送 nil 表示失败
					continue
				}

				// 创建文档对象
				doc := &Document{
					ID:      uuid.New().String(), // 生成唯一 ID
					Content: chunk,
					Metadata: map[string]any{
						"source": source,
						"chunk":  i,
					},
					Embedding: vec,
				}
				results <- doc // 将文档发送到结果通道
				chunkSpan.SetStatus(codes.Ok, "Chunk embedded")
				chunkSpan.End()
			}
		}(w)
	}

	// 分发任务
	for i := 0; i < len(chunks); i++ {
		jobs <- i
	}
	close(jobs) // 关闭任务通道，表示没有更多任务

	// 等待所有工作协程完成
	wg.Wait()
	close(results) // 关闭结果通道

	// 3. 将成功的结果添加到向量存储
	var successfulCount int
	for doc := range results { // 从结果通道收集文档
		if doc != nil {
			a.vectorStore.Add(*doc) // 添加到向量存储
			successfulCount++
		}
	}

	Logger.Info().Int("successful_chunks", successfulCount).Int("total_chunks", len(chunks)).Str("source", source).Msg("Content ingestion finished")

	if successfulCount == 0 && len(chunks) > 0 {
		err := fmt.Errorf("all chunks failed to ingest for source: %s", source)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	span.SetStatus(codes.Ok, "Content ingestion finished")
	return nil
}

// recursiveSplit 递归地将文本分割成块
// chunkSize: 每个块的目标大小
// chunkOverlap: 块之间的重叠字符数
func recursiveSplit(text string, chunkSize int, chunkOverlap int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}

	// 分隔符优先级：段落 -> 行 -> 句子 -> 空格 -> 字符
	separators := []string{"\n\n", "\n", "。 ", ". ", " ", ""}

	var finalChunks []string

	// 内部递归函数，用于按分隔符分割文本
	var split func(text string, separatorIdx int) []string
	split = func(text string, separatorIdx int) []string {
		// 如果文本足够小或者没有更多分隔符，则直接返回文本作为块
		if len(text) <= chunkSize || separatorIdx >= len(separators) {
			// 如果文本仍然太大，并且没有更多分隔符，则按字符分割
			if len(text) > chunkSize && separatorIdx >= len(separators)-1 {
				var parts []string
				runes := []rune(text) // 将字符串转换为 rune 切片以正确处理 Unicode 字符
				for i := 0; i < len(runes); i += chunkSize - chunkOverlap {
					end := i + chunkSize
					if end > len(runes) {
						end = len(runes)
					}
					parts = append(parts, string(runes[i:end]))
				}
				return parts
			}
			return []string{text}
		}

		separator := separators[separatorIdx]
		var parts []string
		if separator == "" { // 应该由上面的检查处理，但作为备用
			return []string{text}
		}

		parts = strings.Split(text, separator) // 按当前分隔符分割
		var result []string
		var currentChunk string // 当前正在构建的块

		for _, part := range parts {
			// 除了最后一个部分，重新添加分隔符以保持上下文
			partWithSep := part
			if len(result) < len(parts)-1 {
				partWithSep += separator
			}

			// 如果添加当前部分会导致块过大
			if len(currentChunk)+len(partWithSep) > chunkSize {
				if currentChunk != "" {
					result = append(result, currentChunk) // 添加当前块到结果
				}
				// 如果当前部分本身大于块大小，则进一步分割
				if len(partWithSep) > chunkSize {
					subParts := split(partWithSep, separatorIdx+1) // 递归调用下一个分隔符
					result = append(result, subParts...)
				} else {
					currentChunk = partWithSep // 否则，将当前部分作为新块的开始
				}
			} else {
				currentChunk += partWithSep // 添加当前部分到当前块
			}
		}
		if currentChunk != "" {
			result = append(result, currentChunk) // 添加最后一个块
		}
		return result
	}

	finalChunks = split(text, 0) // 从第一个分隔符开始分割

	// 后处理：移除空或只包含空白字符的块
	var cleanChunks []string
	for _, c := range finalChunks {
		c = strings.TrimSpace(c)
		if c != "" {
			cleanChunks = append(cleanChunks, c)
		}
	}
	return cleanChunks
}
