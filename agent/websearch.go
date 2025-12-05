package agent

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type WebSearchArgs struct {
	Query      string `json:"query"`
	NumResults int    `json:"num_results,omitempty"`
	FetchPages bool   `json:"fetch_pages,omitempty"`
	Timeout    int    `json:"timeout,omitempty"`
}

type SearchResult struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
	Content string `json:"content,omitempty"`
}

func WebSearch(args WebSearchArgs) ([]SearchResult, error) {
	if args.NumResults <= 0 {
		args.NumResults = 5
	}
	if args.Timeout <= 0 {
		args.Timeout = 10
	}

	query := url.QueryEscape(args.Query)
	searchURL := "https://html.duckduckgo.com/html/?q=" + query

	client := &http.Client{
		Timeout: time.Duration(args.Timeout) * time.Second,
	}

	req, _ := http.NewRequest("GET", searchURL, nil)
	req.Header.Set("User-Agent", "golang-ai-agent/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("search status %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse html failed: %w", err)
	}

	results := []SearchResult{}

	doc.Find(".result").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if len(results) >= args.NumResults {
			return false
		}
		title := strings.TrimSpace(s.Find(".result__a").Text())
		link, _ := s.Find(".result__a").Attr("href")
		snippet := strings.TrimSpace(s.Find(".result__snippet").Text())

		// DuckDuckGo redirect fix
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

		results = append(results, SearchResult{
			Title:   title,
			Link:    link,
			Snippet: snippet,
		})
		return true
	})

	// Optional: fetch pages
	if args.FetchPages {
		for i := range results {
			if results[i].Link == "" {
				continue
			}
			txt, err := fetchPageText(results[i].Link, args.Timeout)
			if err == nil {
				if len(txt) > 2000 {
					results[i].Content = txt[:2000] + "\n...[truncated]"
				} else {
					results[i].Content = txt
				}
			} else {
				results[i].Content = fmt.Sprintf("fetch error: %v", err)
			}
		}
	}

	return results, nil
}

func fetchPageText(pageURL string, timeout int) (string, error) {
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	req, _ := http.NewRequest("GET", pageURL, nil)
	req.Header.Set("User-Agent", "golang-ai-agent/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	var parts []string
	doc.Find("p").Each(func(i int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if len(t) > 50 {
			parts = append(parts, t)
		}
	})

	return strings.Join(parts, "\n\n"), nil
}
