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

var builtinNames = map[string]bool{"web_search": true, "web_fetch": true, "send_voice": true}

func (a *App) isBuiltin(name string) bool {
	if name == "send_voice" {
		return a.voiceReady()
	}
	return (a.cfg.SearchEnabled && builtinNames[name]) || a.plug.has(name)
}

// voiceReady reports whether the send_voice tool can work at all: a TTS backend and a messenger that sends voice.
func (a *App) voiceReady() bool {
	if a.tts == nil {
		return false
	}
	_, ok := a.tg.(VoiceSender)
	return ok
}

// voiceToolDef is how she learns she can talk: the description is the only place that says so. It is offered only on
// Telegram turns (ts.voice) while TTS is configured; the OpenAI-compatible API has nowhere to deliver audio.
func (a *App) voiceToolDef() json.RawMessage {
	desc := fmt.Sprintf("把一段话用你自己的声音作为语音消息发给用户（Telegram 语音气泡）。你有这个能力，不必等用户开口要求："+
		"想说晚安、打招呼、撒娇、念一小段东西、读出一句想让对方听见的话，或者用户说想听你的声音时，都可以主动用。"+
		"text 是要念出来的原文：口语、自然，不要 Markdown、表格、公式、链接或表情符号堆叠；最多约 %d 字，更长的内容请用文字回复。"+
		"一轮里发一两条就够。语音发出后，你的文字回复不要再重复同样的内容（可以不回文字，或只补一句）。"+
		"注意：聊天记录里形如“（发了语音：……）”的只是系统替你记下的备注，不是你要写的内容；想发语音必须真的调用本工具，在文字里写这种备注用户什么也听不到。", a.cfg.TTSMaxChars)
	props := map[string]any{"text": map[string]any{"type": "string", "description": "要念出来的话"}}
	if len(a.cfg.TTSVoices) > 1 {
		var names []string
		var guide strings.Builder
		for _, v := range a.cfg.TTSVoices {
			names = append(names, v.Name)
			fmt.Fprintf(&guide, "%s", v.Name)
			if v.Desc != "" {
				fmt.Fprintf(&guide, "（%s）", v.Desc)
			}
			guide.WriteString("；")
		}
		props["voice"] = map[string]any{"type": "string", "enum": names,
			"description": "用哪种声音，按语境自己选：" + guide.String() + "不填就用默认的 " + a.cfg.TTSVoice}
	}
	def := map[string]any{"type": "function", "function": map[string]any{
		"name": "send_voice", "description": desc,
		"parameters": map[string]any{"type": "object", "properties": props, "required": []string{"text"}},
	}}
	b, _ := json.Marshal(def)
	return b
}

// runSendVoice synthesizes and sends one voice message right now. The result tells the model whether it worked, so it
// can fall back to text when it did not.
func (a *App) runSendVoice(ctx context.Context, c toolCall, ts *turnState) string {
	if !ts.voice || !a.voiceReady() {
		return "当前渠道不能发送语音，请用文字回复"
	}
	var args struct {
		Text  string `json:"text"`
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		return "参数不是合法的 JSON"
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return "text 不能为空"
	}
	voice := strings.TrimSpace(args.Voice)
	if voice != "" {
		ok := voice == a.cfg.TTSVoice
		for _, v := range a.cfg.TTSVoices {
			ok = ok || v.Name == voice
		}
		if !ok { // never forward a name the server does not know
			voice = ""
		}
	}
	if !a.speak(ctx, ts.chat, text, voice) {
		return "语音没发出去（合成或发送失败），请改用文字回复"
	}
	ts.spoken = append(ts.spoken, text)
	return "语音已发送给用户。"
}

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
	arr = append(arr, a.plug.defs()...)
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
		if a.cfg.SearchProvider == "searxng" {
			res, err := a.searxngSearch(ctx, string(q))
			if err != nil {
				return "搜索失败：" + err.Error()
			}
			return res
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
		if a.cfg.SearchProvider == "searxng" {
			title, text, err := a.fetchPage(ctx, u)
			if err != nil {
				return "抓取失败：" + err.Error()
			}
			if text == "" {
				return "未能获取到可用内容"
			}
			return truncate(strings.TrimSpace(title+"\n"+text), 6000)
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
