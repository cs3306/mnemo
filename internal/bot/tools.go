package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Built-in tools the model may call when SEARCH_ENABLED=true. They run on the server through
// DeepSeek's Search/Fetch API; a request to those endpoints sends the query/URL to that service.
const searchToolDefs = `[
 {"type":"function","function":{"name":"web_search","description":"Search the web for current information. Use when the answer depends on recent or external facts you do not know.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"search query, at most 128 characters"}},"required":["query"]}}},
 {"type":"function","function":{"name":"web_fetch","description":"Fetch one web page (http/https) and return its text as Markdown.","parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}}}
]`

var builtinNames = map[string]bool{"web_search": true, "web_fetch": true}

func (a *App) isBuiltin(name string) bool { return a.cfg.SearchEnabled && builtinNames[name] }

// mergeTools combines client-declared tools with the built-in ones (nil when there are none).
func (a *App) mergeTools(client json.RawMessage) (json.RawMessage, error) {
	var arr []json.RawMessage
	if len(bytes.TrimSpace(client)) > 0 && string(bytes.TrimSpace(client)) != "null" {
		if err := json.Unmarshal(client, &arr); err != nil {
			return nil, errors.New("tools must be a JSON array")
		}
	}
	if a.cfg.SearchEnabled {
		var b []json.RawMessage
		_ = json.Unmarshal([]byte(searchToolDefs), &b)
		arr = append(arr, b...)
	}
	if len(arr) == 0 {
		return nil, nil
	}
	return json.Marshal(arr)
}

type toolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (a *App) searchPost(ctx context.Context, path string, body, out any) error {
	b, _ := json.Marshal(body)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, a.cfg.SearchBase+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.SearchKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.SearchKey)
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return errors.New("network error")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		switch resp.StatusCode {
		case 402:
			return errors.New("余额不足")
		case 403:
			return errors.New("该页面被安全策略拦截")
		case 404:
			return errors.New("未能获取到可用内容")
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

// runBuiltin executes a built-in tool. Failures are returned as text so the model can react; they never abort the turn.
func (a *App) runBuiltin(ctx context.Context, c toolCall) string {
	var args struct {
		Query string `json:"query"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		return "参数不是合法的 JSON"
	}
	switch c.Function.Name {
	case "web_search":
		q := []rune(strings.TrimSpace(args.Query))
		if len(q) == 0 {
			return "query 不能为空"
		}
		if len(q) > 128 {
			q = q[:128]
		}
		var out struct {
			Results []struct {
				Title    string `json:"title"`
				URL      string `json:"url"`
				Snippet  string `json:"snippet"`
				Summary  string `json:"summary"`
				SiteName string `json:"site_name"`
				Date     string `json:"date"`
			} `json:"results"`
		}
		if err := a.searchPost(ctx, "/v1/tools/search", map[string]any{"query": string(q), "count": 5}, &out); err != nil {
			return "搜索失败：" + err.Error()
		}
		if len(out.Results) == 0 {
			return "没有搜索结果"
		}
		var b strings.Builder
		for i, r := range out.Results {
			body := r.Summary
			if body == "" {
				body = r.Snippet
			}
			fmt.Fprintf(&b, "%d. %s (%s %s)\n%s\n%s\n\n", i+1, r.Title, r.SiteName, r.Date, r.URL, truncate(body, 500))
		}
		return strings.TrimSpace(b.String())
	case "web_fetch":
		u, err := url.Parse(strings.TrimSpace(args.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return "url 必须是不含账号密码的 http(s) 链接"
		}
		var out struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := a.searchPost(ctx, "/v1/tools/fetch", map[string]any{"url": u.String()}, &out); err != nil {
			return "抓取失败：" + err.Error()
		}
		return truncate(strings.TrimSpace(out.Title+"\n"+out.Content), 6000)
	}
	return "未知工具：" + c.Function.Name
}
