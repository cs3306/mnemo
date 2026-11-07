package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tool plugins are separate HTTP services (any language) that expose tools to the model.
//
//	GET  {base}/v1/tools              -> {"tools":[{"name","description","parameters":<JSON Schema object>}]}
//	POST {base}/v1/tools/{name}       <- {"arguments":{...},"context":{"chat":123,"channel":"telegram"|"api","tz":"America/Vancouver"}}
//	                                  -> {"text":"...","files":[{"name","mime","data":"<base64>"}]}  (or {"error":"..."})
//
// Every request carries "Authorization: Bearer $PLUGIN_TOKEN" when PLUGIN_TOKEN is set. Files are delivered to the
// user over Telegram; on the API channel they are dropped and the model is told so.
const (
	pluginMaxResponse   = 96 << 20
	pluginMaxFile       = 40 << 20
	pluginMaxFiles      = 8
	pluginMaxText       = 8000
	pluginRetryInterval = 60 * time.Second
	pluginDiscoverTO    = 8 * time.Second
)

var pluginNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

// OutFile is a file produced by a tool, to be delivered to the user.
type OutFile struct {
	Name string
	Mime string
	Data []byte
}

// FileSender is implemented by messengers that can upload files (the Telegram client does).
type FileSender interface {
	SendFile(ctx context.Context, chat int64, name, mime string, data []byte) error
}

type pluginState struct {
	tools   []string
	lastTry time.Time
	err     string
}

type pluginSet struct {
	mu    sync.Mutex
	disc  sync.Mutex // serialises discovery
	tools map[string]pluginTool
	urls  map[string]*pluginState
}

type pluginTool struct {
	base string
	def  json.RawMessage // OpenAI-format tool definition
}

func newPluginSet(urls []string) *pluginSet {
	p := &pluginSet{tools: map[string]pluginTool{}, urls: map[string]*pluginState{}}
	for _, u := range urls {
		p.urls[u] = &pluginState{}
	}
	return p
}

func (p *pluginSet) has(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.tools[name]
	return ok
}

func (p *pluginSet) defs() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.tools))
	for n := range p.tools {
		names = append(names, n)
	}
	sort.Strings(names) // a stable order keeps the prompt prefix (and the provider's cache) stable
	out := make([]json.RawMessage, 0, len(names))
	for _, n := range names {
		out = append(out, p.tools[n].def)
	}
	return out
}

func (a *App) pluginReq(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.cfg.PluginToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.PluginToken)
	}
	return req, nil
}

// discoverURL (re)loads the tool list of one plugin server. On failure the previously known tools stay in place.
func (a *App) discoverURL(ctx context.Context, base string) error {
	p := a.plug
	ctx, cancel := context.WithTimeout(ctx, pluginDiscoverTO)
	defer cancel()
	fail := func(err error) error {
		p.mu.Lock()
		if st := p.urls[base]; st != nil {
			st.lastTry, st.err = a.now(), err.Error()
		}
		p.mu.Unlock()
		return err
	}
	req, err := a.pluginReq(ctx, http.MethodGet, base+"/v1/tools", nil)
	if err != nil {
		return fail(err)
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return fail(cleanNetErr(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fail(fmt.Errorf("HTTP %d：PLUGIN_TOKEN 不正确？", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return fail(fmt.Errorf("返回的不是 {\"tools\":[…]} JSON"))
	}
	fresh := map[string]pluginTool{}
	for _, t := range list.Tools {
		switch {
		case !pluginNameRE.MatchString(t.Name):
			slog.Warn("plugin tool skipped: invalid name", "plugin", base, "name", t.Name)
			continue
		case builtinNames[t.Name]:
			slog.Warn("plugin tool skipped: name reserved by a built-in tool", "plugin", base, "name", t.Name)
			continue
		}
		params := t.Parameters
		var probe map[string]any
		if len(params) == 0 || json.Unmarshal(params, &probe) != nil || probe == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		def, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": truncate(t.Description, 2000), "parameters": params,
		}})
		fresh[t.Name] = pluginTool{base: base, def: def}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for n, t := range p.tools { // drop what this server used to offer
		if t.base == base {
			delete(p.tools, n)
		}
	}
	st := p.urls[base]
	st.tools, st.lastTry, st.err = nil, a.now(), ""
	for n, t := range fresh {
		if old, taken := p.tools[n]; taken {
			slog.Warn("plugin tool skipped: name already provided by another plugin server", "name", n, "kept", old.base, "skipped", base)
			continue
		}
		p.tools[n] = t
		st.tools = append(st.tools, n)
	}
	sort.Strings(st.tools)
	return nil
}

// discoverAll loads every configured server, returning a human-readable status.
func (a *App) discoverAll(ctx context.Context) string {
	a.plug.disc.Lock()
	defer a.plug.disc.Unlock()
	var urls []string
	for u := range a.plug.urls {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	for _, u := range urls {
		if err := a.discoverURL(ctx, u); err != nil {
			slog.Warn("plugin discovery failed", "plugin", u, "err", err)
		}
	}
	return a.pluginStatus()
}

// ensurePlugins retries servers that have offered nothing yet (down at startup, say), at most once a minute each.
func (a *App) ensurePlugins(ctx context.Context) {
	if len(a.plug.urls) == 0 {
		return
	}
	a.plug.mu.Lock()
	var todo []string
	for u, st := range a.plug.urls {
		if len(st.tools) == 0 && a.now().Sub(st.lastTry) >= pluginRetryInterval {
			todo = append(todo, u)
		}
	}
	a.plug.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	a.plug.disc.Lock()
	defer a.plug.disc.Unlock()
	for _, u := range todo {
		_ = a.discoverURL(ctx, u)
	}
}

func (a *App) pluginStatus() string {
	p := a.plug
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.urls) == 0 {
		return "没有配置插件（PLUGIN_URLS 为空）。"
	}
	var urls []string
	for u := range p.urls {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	var b strings.Builder
	for _, u := range urls {
		st := p.urls[u]
		switch {
		case st.err != "":
			fmt.Fprintf(&b, "✗ %s\n   %s\n", u, truncate(st.err, 160))
		case st.lastTry.IsZero():
			fmt.Fprintf(&b, "… %s（尚未连接）\n", u)
		default:
			fmt.Fprintf(&b, "✓ %s\n   %s\n", u, strings.Join(st.tools, ", "))
		}
	}
	return strings.TrimSpace(b.String())
}

// turnState is what a tool call needs to know about the turn it runs in, and where its files go.
type turnState struct {
	chat    int64
	deliver bool // the frontend can send files (Telegram with a file-capable messenger)
	files   []OutFile
}

func (a *App) canSendFiles() bool {
	_, ok := a.tg.(FileSender)
	return ok
}

// runTool runs a plugin tool or, failing that, a built-in one. Failures come back as text for the model.
func (a *App) runTool(ctx context.Context, c toolCall, ts *turnState) string {
	if a.plug.has(c.Function.Name) {
		return a.callPlugin(ctx, c, ts)
	}
	return a.runBuiltin(ctx, c)
}

func (a *App) callPlugin(ctx context.Context, c toolCall, ts *turnState) string {
	a.plug.mu.Lock()
	t := a.plug.tools[c.Function.Name]
	a.plug.mu.Unlock()
	args := json.RawMessage(strings.TrimSpace(c.Function.Arguments))
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var probe map[string]any
	if json.Unmarshal(args, &probe) != nil || probe == nil {
		return "参数不是合法的 JSON 对象"
	}
	channel := "api"
	if ts.deliver {
		channel = "telegram"
	}
	body, _ := json.Marshal(map[string]any{"arguments": args, "context": map[string]any{"chat": ts.chat, "channel": channel, "tz": a.cfg.Loc.String()}})
	cctx, cancel := context.WithTimeout(ctx, a.cfg.PluginTimeout)
	defer cancel()
	req, err := a.pluginReq(cctx, http.MethodPost, t.base+"/v1/tools/"+c.Function.Name, body)
	if err != nil {
		return "插件调用失败：" + err.Error()
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return "插件调用失败：" + cleanNetErr(err).Error()
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, pluginMaxResponse+1))
	if err != nil {
		return "插件调用失败：" + cleanNetErr(err).Error()
	}
	if len(b) > pluginMaxResponse {
		return "插件调用失败：返回内容过大"
	}
	var out struct {
		Text  string `json:"text"`
		Error string `json:"error"`
		Files []struct {
			Name string `json:"name"`
			Mime string `json:"mime"`
			Data []byte `json:"data"` // base64 in JSON
		} `json:"files"`
	}
	jerr := json.Unmarshal(b, &out)
	if resp.StatusCode != http.StatusOK || out.Error != "" {
		msg := out.Error
		if msg == "" {
			msg = truncate(strings.TrimSpace(string(b)), 200)
		}
		return fmt.Sprintf("插件调用失败（HTTP %d）：%s", resp.StatusCode, msg)
	}
	if jerr != nil {
		return "插件调用失败：返回的不是合法 JSON"
	}
	var notes []string
	for i, f := range out.Files {
		name := safeFileName(f.Name)
		switch {
		case i >= pluginMaxFiles:
			notes = append(notes, "[文件过多，已忽略："+name+"]")
		case len(f.Data) == 0:
			notes = append(notes, "[空文件，已忽略："+name+"]")
		case len(f.Data) > pluginMaxFile:
			notes = append(notes, "[文件超过大小上限，已忽略："+name+"]")
		case !ts.deliver:
			notes = append(notes, "[已生成 "+name+"，但当前渠道无法发送文件，请告诉用户]")
		default:
			mime := f.Mime
			if mime == "" {
				mime = "application/octet-stream"
			}
			ts.files = append(ts.files, OutFile{Name: name, Mime: mime, Data: f.Data})
			notes = append(notes, "[文件已发送给用户："+name+"]")
		}
	}
	res := truncate(strings.TrimSpace(out.Text), pluginMaxText)
	if len(notes) > 0 {
		res = strings.TrimSpace(res + "\n" + strings.Join(notes, "\n"))
	}
	if res == "" {
		res = "完成（无输出）"
	}
	return res
}

// safeFileName keeps only a plain base name: no directories, no control characters.
func safeFileName(n string) string {
	n = filepath.Base(strings.ReplaceAll(strings.TrimSpace(n), "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, n)
	if n == "" || n == "." || n == ".." || n == "/" {
		return "file"
	}
	if r := []rune(n); len(r) > 100 {
		n = string(r[:100])
	}
	return n
}
