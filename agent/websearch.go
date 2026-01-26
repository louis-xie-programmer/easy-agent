// websearch.go
// agent 包中的网页搜索工具模块，负责：
// - 定义网页搜索的参数和结果结构
// - 实现通过 DuckDuckGo 进行网页搜索的功能
// - 支持抓取搜索结果页面的内容
package agent

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// WebSearchArgs 定义了网页搜索工具的参数结构
type WebSearchArgs struct {
	Query      string `json:"query"`                 // 搜索查询字符串
	NumResults int    `json:"num_results,omitempty"` // 返回的搜索结果数量，可选
	FetchPages bool   `json:"fetch_pages,omitempty"` // 是否抓取搜索结果页面的完整内容，可选
	Timeout    int    `json:"timeout,omitempty"`     // 搜索请求的超时时间（秒），可选
}

// WebSearchResult 定义了单个网页搜索结果的结构
// 重命名以避免与 vector_store.go 中的 SearchResult 冲突
type WebSearchResult struct {
	Title   string `json:"title"`             // 搜索结果的标题
	Link    string `json:"link"`              // 搜索结果的链接 URL
	Snippet string `json:"snippet"`           // 搜索结果的摘要
	Content string `json:"content,omitempty"` // 抓取到的页面完整内容，如果 FetchPages 为 true
}

// WebSearch 执行网页搜索，使用 DuckDuckGo 的 HTML 接口
// args: 网页搜索的参数
// 返回搜索结果列表和可能发生的错误
func WebSearch(args WebSearchArgs) ([]WebSearchResult, error) {
	Logger.Info().Str("query", args.Query).Msg("Executing web_search tool")
	if args.NumResults <= 0 {
		args.NumResults = 10 // 默认返回 10 个结果
	}
	if args.Timeout <= 0 {
		args.Timeout = 15 // 默认超时 15 秒
	}

	query := url.QueryEscape(args.Query)                        // 对查询字符串进行 URL 编码
	searchURL := "https://html.duckduckgo.com/html/?q=" + query // DuckDuckGo HTML 搜索接口

	// 创建带有超时设置的 HTTP 客户端
	client := &http.Client{
		Timeout: time.Duration(args.Timeout) * time.Second,
	}

	// 创建 HTTP GET 请求
	req, _ := http.NewRequest("GET", searchURL, nil)
	req.Header.Set("User-Agent", "golang-ai-agent/1.0") // 设置 User-Agent

	// 发送搜索请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close() // 确保响应体关闭

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("search status %d", resp.StatusCode)
	}

	// 使用 goquery 解析 HTML 响应
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse html failed: %w", err)
	}

	var results []WebSearchResult

	// 遍历搜索结果，提取标题、链接和摘要
	doc.Find(".result").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if len(results) >= args.NumResults {
			return false // 达到指定结果数量，停止遍历
		}
		title := strings.TrimSpace(s.Find(".result__a").Text())
		link, _ := s.Find(".result__a").Attr("href")
		snippet := strings.TrimSpace(s.Find(".result__snippet").Text())

		// 修复 DuckDuckGo 的重定向链接
		if strings.Contains(link, "uddg=") {
			u, err := url.Parse(link)
			if err == nil {
				if q, err2 := url.ParseQuery(u.RawQuery); err2 == nil {
					if encoded := q.Get("uddg"); encoded != "" {
						if decoded, err3 := url.QueryUnescape(encoded); err3 == nil {
							link = decoded
						}
					}
				}
			}
		}

		results = append(results, WebSearchResult{
			Title:   title,
			Link:    link,
			Snippet: snippet,
		})
		return true
	})

	// 如果请求抓取页面内容且有搜索结果，则并发抓取页面
	if args.FetchPages && len(results) > 0 {
		var wg sync.WaitGroup
		wg.Add(len(results))

		for i := range results {
			go func(idx int) {
				defer wg.Done()
				if results[idx].Link == "" {
					return
				}
				txt, err := fetchPageText(results[idx].Link, args.Timeout) // 抓取页面文本
				if err == nil {
					// 将页面内容截断到合理长度
					const maxContentLength = 4000
					if len(txt) > maxContentLength {
						results[idx].Content = txt[:maxContentLength] + "\n...[truncated]"
					} else {
						results[idx].Content = txt
					}
				} else {
					results[idx].Content = fmt.Sprintf("fetch error: %v", err) // 记录抓取错误
				}
			}(i)
		}
		wg.Wait() // 等待所有页面抓取完成
	}

	return results, nil
}

// fetchPageText 抓取指定 URL 的页面文本内容
// pageURL: 要抓取的页面 URL
// timeout: HTTP 请求超时时间（秒）
// 返回页面文本内容和可能发生的错误
func fetchPageText(pageURL string, timeout int) (string, error) {
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second} // 创建带有超时设置的 HTTP 客户端

	req, _ := http.NewRequest("GET", pageURL, nil)
	req.Header.Set("User-Agent", "golang-ai-agent/1.0") // 设置 User-Agent

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() // 确保响应体关闭

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("failed with status: %d", resp.StatusCode)
	}

	// 使用 goquery 解析 HTML 响应
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	// 更健壮地提取文本内容：移除不必要的元素（如脚本、样式、导航等）
	doc.Find("script, style, nav, header, footer, aside").Remove()

	var sb strings.Builder
	// 遍历 body 元素，提取所有文本
	doc.Find("body").Each(func(i int, s *goquery.Selection) {
		sb.WriteString(s.Text())
	})

	// 规范化空白字符：将多个连续的空白字符替换为单个空格
	text := strings.Join(strings.Fields(sb.String()), " ")

	return text, nil
}
