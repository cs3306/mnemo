package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/store"
	"companion/internal/testutil"
)

type fileTG struct {
	fakeTG
	fmu   sync.Mutex
	files []OutFile
	chats []int64
}

func (f *fileTG) SendFile(_ context.Context, chat int64, name, mime string, data []byte) error {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.files = append(f.files, OutFile{Name: name, Mime: mime, Data: data})
	f.chats = append(f.chats, chat)
	return nil
}

// fakePlugin serves the plugin protocol. handler(name, body) returns the JSON to answer a call with.
type fakePlugin struct {
	*httptest.Server
	token    string
	tools    string
	listHits int32
	down     atomic.Bool
	last     atomic.Value // last call body (map[string]any)
	auth     atomic.Value
	handler  func(name string, body map[string]any) (int, any)
}

func newFakePlugin(t *testing.T, token, tools string) *fakePlugin {
	p := &fakePlugin{token: token, tools: tools}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.auth.Store(r.Header.Get("Authorization"))
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/tools" {
			atomic.AddInt32(&p.listHits, 1)
			if p.down.Load() {
				http.Error(w, "starting", 503)
				return
			}
			w.Write([]byte(p.tools))
			return
		}
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/tools/") {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			p.last.Store(body)
			code, out := p.handler(strings.TrimPrefix(r.URL.Path, "/v1/tools/"), body)
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(out)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

const pluginTools = `{"tools":[
 {"name":"make_doc","description":"创建 Word 文档","parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}},
 {"name":"1bad","description":"invalid name","parameters":{}},
 {"name":"web_search","description":"reserved","parameters":{}},
 {"name":"noparams","description":"no schema given"}
]}`

func callTool(name, args string) llm.Result {
	b, _ := json.Marshal([]map[string]any{{"id": "p1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}})
	return llm.Result{ToolCalls: b}
}

func newPluginApp(t *testing.T, chat llm.Completer, tg Messenger, urls []string, token string, mut func(*config.Config)) *App {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": dir, "ALLOWED_USER_IDS": "42", "EXTRACT_EVERY": "100", "LLM_MODEL": "m", "API_KEY": "secret",
			"PLUGIN_URLS": strings.Join(urls, ","), "PLUGIN_TOKEN": token}[k]
	})
	if mut != nil {
		mut(cfg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app, err := New(cfg, Deps{Store: st, Chat: chat, Maint: &testutil.Chat{}, Embed: &testutil.Embed{Name: "e"}, TG: tg, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func toolNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var arr []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	var out []string
	for _, a := range arr {
		out = append(out, a.Function.Name)
	}
	return out
}

func TestPluginFilesAreSentOnTelegram(t *testing.T) {
	docx := []byte("PK-fake-docx")
	p := newFakePlugin(t, "tok", pluginTools)
	p.handler = func(name string, body map[string]any) (int, any) {
		return 200, map[string]any{"text": "已生成 周报", "files": []map[string]any{
			{"name": "../../etc/周报.docx", "mime": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "data": base64.StdEncoding.EncodeToString(docx)},
		}}
	}
	chat := &testutil.ToolChat{Script: []llm.Result{callTool("make_doc", `{"title":"周报"}`), {Text: "文档做好啦，发给你了。"}}}
	tg := &fileTG{}
	app := newPluginApp(t, chat, tg, []string{p.URL}, "tok", nil)
	app.discoverAll(context.Background())

	app.Handle(context.Background(), msg(42, "帮我写周报"))

	if got := tg.last(); got != "文档做好啦，发给你了。" {
		t.Fatalf("reply %q", got)
	}
	if len(tg.files) != 1 || tg.files[0].Name != "周报.docx" || string(tg.files[0].Data) != string(docx) || tg.chats[0] != 42 {
		t.Fatalf("files: %+v chats %v", tg.files, tg.chats)
	}
	// the plugin got the arguments, the channel and the bearer token
	body := p.last.Load().(map[string]any)
	if body["arguments"].(map[string]any)["title"] != "周报" || body["context"].(map[string]any)["channel"] != "telegram" || body["context"].(map[string]any)["chat"].(float64) != 42 {
		t.Fatalf("body %v", body)
	}
	if p.auth.Load() != "Bearer tok" {
		t.Fatalf("auth %v", p.auth.Load())
	}
	// the model saw the result and knew the file was sent
	if got := joinContents(chat.Calls[1]); !strings.Contains(got, "已生成 周报") || !strings.Contains(got, "文件已发送给用户：周报.docx") {
		t.Fatalf("tool result not given to model: %s", got)
	}
	// only valid, non-reserved plugin tools were offered, in addition to nothing else
	if names := toolNames(t, chat.ToolsSeen[0]); strings.Join(names, ",") != "make_doc,noparams" {
		t.Fatalf("offered %v", names)
	}
	// the plugin exchange is not stored as conversation history
	if strings.Contains(joinContents(chat.Calls[1]), "PK-fake") {
		t.Fatal("file bytes must never reach the model")
	}
}

func TestPluginFilesAreNotSentOnAPI(t *testing.T) {
	p := newFakePlugin(t, "", pluginTools)
	p.handler = func(string, map[string]any) (int, any) {
		return 200, map[string]any{"text": "ok", "files": []map[string]any{{"name": "a.pdf", "data": base64.StdEncoding.EncodeToString([]byte("%PDF"))}}}
	}
	chat := &testutil.ToolChat{Script: []llm.Result{callTool("make_doc", `{"title":"x"}`), {Text: "无法发送文件，请在 Telegram 里使用。"}}}
	_, srv, _ := newToolApp(t, chat, nil, func(c *config.Config) { c.PluginURLs, c.MaxToolRounds = []string{p.URL}, 4 })
	// newToolApp builds the app before we can discover, so discover through a second request path:
	// the first turn triggers ensurePlugins, which finds the tools.
	finish, msg := decode(t, post(t, srv, "secret", `{"messages":[{"role":"user","content":"做个pdf"}]}`))
	if finish != "stop" || msg["content"] != "无法发送文件，请在 Telegram 里使用。" {
		t.Fatalf("%q %v", finish, msg)
	}
	if got := joinContents(chat.Calls[1]); !strings.Contains(got, "当前渠道无法发送文件") {
		t.Fatalf("model must be told the file cannot be delivered: %s", got)
	}
	if body := p.last.Load().(map[string]any); body["context"].(map[string]any)["channel"] != "api" {
		t.Fatalf("%v", body)
	}
}

func TestPluginFailuresComeBackAsText(t *testing.T) {
	cases := []struct {
		name    string
		handler func(string, map[string]any) (int, any)
		args    string
		want    string
	}{
		{"error field", func(string, map[string]any) (int, any) { return 400, map[string]any{"error": "title 太长"} }, `{"title":"x"}`, "title 太长"},
		{"500 plain", func(string, map[string]any) (int, any) { return 500, "boom" }, `{"title":"x"}`, "HTTP 500"},
		{"200 with error", func(string, map[string]any) (int, any) { return 200, map[string]any{"error": "soft fail"} }, `{"title":"x"}`, "soft fail"},
		{"args not an object", nil, `[1,2]`, "合法的 JSON 对象"},
		{"empty result", func(string, map[string]any) (int, any) { return 200, map[string]any{} }, `{}`, "完成（无输出）"},
	}
	for _, c := range cases {
		p := newFakePlugin(t, "", pluginTools)
		p.handler = c.handler
		app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, &fakeTG{}, []string{p.URL}, "", nil)
		app.discoverAll(context.Background())
		out := runPluginTool(app, "make_doc", c.args)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: %q", c.name, out)
		}
	}
}

func runPluginTool(app *App, name, args string) string {
	var c toolCall
	c.ID = "t"
	c.Function.Name, c.Function.Arguments = name, args
	return app.runTool(context.Background(), c, &turnState{chat: 1})
}

func TestPluginTimeout(t *testing.T) {
	p := newFakePlugin(t, "", pluginTools)
	p.handler = func(string, map[string]any) (int, any) {
		time.Sleep(300 * time.Millisecond)
		return 200, map[string]any{"text": "late"}
	}
	app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, &fakeTG{}, []string{p.URL}, "", func(c *config.Config) { c.PluginTimeout = 50 * time.Millisecond })
	app.discoverAll(context.Background())
	if out := runPluginTool(app, "make_doc", `{}`); !strings.Contains(out, "插件调用失败") {
		t.Fatalf("%q", out)
	}
}

func TestPluginFileLimits(t *testing.T) {
	p := newFakePlugin(t, "", pluginTools)
	var files []map[string]any
	for i := 0; i < 10; i++ {
		files = append(files, map[string]any{"name": "f" + string(rune('0'+i)) + ".txt", "data": base64.StdEncoding.EncodeToString([]byte("x"))})
	}
	files[1]["data"] = "" // empty
	p.handler = func(string, map[string]any) (int, any) { return 200, map[string]any{"text": "ok", "files": files} }
	app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, &fileTG{}, []string{p.URL}, "", nil)
	app.discoverAll(context.Background())
	ts := &turnState{chat: 1, deliver: true}
	var c toolCall
	c.Function.Name, c.Function.Arguments = "make_doc", `{}`
	out := app.runTool(context.Background(), c, ts)
	if len(ts.files) != pluginMaxFiles-1 || !strings.Contains(out, "空文件") || !strings.Contains(out, "文件过多") {
		t.Fatalf("delivered %d: %s", len(ts.files), out)
	}
}

func TestPluginDiscoveryRetriesAndStatus(t *testing.T) {
	p := newFakePlugin(t, "tok", pluginTools)
	p.down.Store(true)
	now := time.Now()
	chat := &testutil.ToolChat{Script: []llm.Result{{Text: "好"}}}
	app := newPluginApp(t, chat, &fileTG{}, []string{p.URL}, "tok", nil)
	app.now = func() time.Time { return now }

	app.Handle(context.Background(), msg(42, "你好")) // discovery fails: no tools, conversation still works
	if len(chat.ToolsSeen[0]) != 0 || atomic.LoadInt32(&p.listHits) != 1 {
		t.Fatalf("tools %s hits %d", chat.ToolsSeen[0], p.listHits)
	}
	app.Handle(context.Background(), msg(42, "再来")) // within a minute: no new attempt
	if atomic.LoadInt32(&p.listHits) != 1 {
		t.Fatalf("retried too early: %d", p.listHits)
	}
	p.down.Store(false)
	now = now.Add(61 * time.Second)
	app.Handle(context.Background(), msg(42, "第三次"))
	if names := toolNames(t, chat.ToolsSeen[len(chat.ToolsSeen)-1]); len(names) != 2 {
		t.Fatalf("tools after recovery: %v", names)
	}
	if st := app.pluginStatus(); !strings.Contains(st, "✓") || !strings.Contains(st, "make_doc") {
		t.Fatalf("%s", st)
	}
}

func TestPluginAuthFailureIsExplained(t *testing.T) {
	p := newFakePlugin(t, "right", pluginTools)
	app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, &fakeTG{}, []string{p.URL}, "wrong", nil)
	st := app.discoverAll(context.Background())
	if !strings.Contains(st, "✗") || !strings.Contains(st, "PLUGIN_TOKEN") {
		t.Fatalf("%s", st)
	}
	if len(app.plug.defs()) != 0 {
		t.Fatal("no tools may be registered after an auth failure")
	}
}

func TestPluginCommand(t *testing.T) {
	p := newFakePlugin(t, "", pluginTools)
	tg := &fileTG{}
	app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, tg, []string{p.URL}, "", nil)
	app.Handle(context.Background(), msg(42, "/plugins reload"))
	app.Handle(context.Background(), msg(42, "/plugins"))
	if got := tg.last(); !strings.Contains(got, p.URL) || !strings.Contains(got, "make_doc, noparams") {
		t.Fatalf("%q", got)
	}
	none := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, tg, nil, "", nil)
	none.Handle(context.Background(), msg(42, "/plugins"))
	if !strings.Contains(tg.last(), "没有配置插件") {
		t.Fatalf("%q", tg.last())
	}
}

func TestPluginToolsKeepStableOrderAndNoDuplicates(t *testing.T) {
	a := newFakePlugin(t, "", `{"tools":[{"name":"zeta","description":"z"},{"name":"alpha","description":"a"}]}`)
	b := newFakePlugin(t, "", `{"tools":[{"name":"alpha","description":"dup"},{"name":"mid","description":"m"}]}`)
	app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "x"}}}, &fakeTG{}, []string{a.URL, b.URL}, "", nil)
	app.discoverAll(context.Background())
	raw, _ := app.mergeTools(nil)
	if names := toolNames(t, raw); strings.Join(names, ",") != "alpha,mid,zeta" {
		t.Fatalf("%v", names)
	}
	// a server that stops offering a tool loses it on the next discovery
	a.tools = `{"tools":[{"name":"zeta","description":"z"}]}`
	app.discoverAll(context.Background())
	raw, _ = app.mergeTools(nil)
	if names := toolNames(t, raw); strings.Join(names, ",") != "alpha,mid,zeta" { // alpha now comes from b
		t.Fatalf("%v", names)
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"a.docx": "a.docx", "../../x/y.pdf": "y.pdf", `C:\tmp\z.xlsx`: "z.xlsx", "": "file", "..": "file", "a\x00b.txt": "ab.txt", "报告 最终版.pptx": "报告 最终版.pptx",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := safeFileName(strings.Repeat("长", 300) + ".txt"); len([]rune(got)) != 100 {
		t.Fatalf("%d", len([]rune(got)))
	}
}

// End-to-end against a real mnemo-plugin server:
//
//	PLUGIN_TOKEN=dev PLUGIN_ADDR=127.0.0.1:18090 python -m mnemo_plugin &
//	MNEMO_PLUGIN_E2E=http://127.0.0.1:18090 MNEMO_PLUGIN_E2E_TOKEN=dev go test -run PluginE2E -v ./internal/bot
func TestPluginE2E(t *testing.T) {
	base := os.Getenv("MNEMO_PLUGIN_E2E")
	if base == "" {
		t.Skip("MNEMO_PLUGIN_E2E not set")
	}
	token := os.Getenv("MNEMO_PLUGIN_E2E_TOKEN")
	calls := []llm.Result{
		callTool("docx_create", `{"filename":"周报","markdown":"# 本周\n\n完成了**线代**作业。$a^2+b^2=c^2$"}`),
		callTool("xlsx_create", `{"filename":"账","sheets":[{"name":"11月","columns":["项目","金额"],"rows":[["午饭",35.5],["合计","=SUM(B2:B2)"]]}]}`),
		callTool("pdf_create", `{"filename":"笔记","markdown":"# 特征值\n\n中文段落。$\\lambda$"}`),
		callTool("latex_create", `{"filename":"t","source":"\\documentclass{article}\\begin{document}Hello\\end{document}"}`),
		{Text: "都做好啦。"},
	}
	chat := &testutil.ToolChat{Script: calls}
	tg := &fileTG{}
	app := newPluginApp(t, chat, tg, []string{base}, token, func(c *config.Config) { c.MaxToolRounds = 8 })
	status := app.discoverAll(context.Background())
	if !strings.Contains(status, "docx_create") {
		t.Fatalf("discovery: %s", status)
	}
	app.Handle(context.Background(), msg(42, "全做一遍"))
	if tg.last() != "都做好啦。" {
		t.Fatalf("reply %q\n%v", tg.last(), tg.sent)
	}
	var names []string
	for _, f := range tg.files {
		names = append(names, f.Name)
		switch {
		case strings.HasSuffix(f.Name, ".docx"), strings.HasSuffix(f.Name, ".xlsx"):
			if !bytes.HasPrefix(f.Data, []byte("PK")) {
				t.Errorf("%s is not a zip", f.Name)
			}
		case strings.HasSuffix(f.Name, ".pdf"):
			if !bytes.HasPrefix(f.Data, []byte("%PDF")) {
				t.Errorf("%s is not a PDF", f.Name)
			}
		}
	}
	if strings.Join(names, ",") != "周报.docx,账.xlsx,笔记.pdf,t.pdf,t.tex" {
		t.Fatalf("files %v", names)
	}
}

func TestPluginNoticeReachesTelegramBeforeTheModelReplies(t *testing.T) {
	p := newFakePlugin(t, "tok", pluginTools)
	p.handler = func(string, map[string]any) (int, any) {
		return 200, map[string]any{"text": "已创建", "notify": "✅ 已创建定时任务「晨报」：每天 07:30"}
	}
	chat := &testutil.ToolChat{Script: []llm.Result{callTool("make_doc", `{"title":"x"}`), {Text: "好了。"}}}
	tg := &fileTG{}
	app := newPluginApp(t, chat, tg, []string{p.URL}, "tok", nil)
	app.discoverAll(context.Background())
	app.Handle(context.Background(), msg(42, "每天7:30晨报"))
	tg.mu.Lock()
	sent := append([]string(nil), tg.sent...)
	tg.mu.Unlock()
	if len(sent) != 2 || sent[0] != "✅ 已创建定时任务「晨报」：每天 07:30" || sent[1] != "好了。" {
		t.Fatalf("messages: %q", sent)
	}
}

func TestPluginNoticeIsNotSentOverTheAPI(t *testing.T) {
	p := newFakePlugin(t, "", pluginTools)
	p.handler = func(string, map[string]any) (int, any) {
		return 200, map[string]any{"text": "ok", "notify": "NOTICE"}
	}
	chat := &testutil.ToolChat{Script: []llm.Result{callTool("make_doc", `{"title":"x"}`), {Text: "完成"}}}
	app, srv, _ := newToolApp(t, chat, nil, func(c *config.Config) { c.PluginURLs, c.MaxToolRounds = []string{p.URL}, 4 })
	tg := &fakeTG{}
	app.tg = tg
	decode(t, post(t, srv, "secret", `{"messages":[{"role":"user","content":"x"}]}`))
	if len(tg.sent) != 0 {
		t.Fatalf("API turns must not message Telegram: %q", tg.sent)
	}
}

func TestAnnouncedButNotDoneActionIsNudgedOnceAndNeverShown(t *testing.T) {
	p := newFakePlugin(t, "tok", pluginTools)
	p.handler = func(string, map[string]any) (int, any) { return 200, map[string]any{"text": "已创建"} }
	chat := &testutil.ToolChat{Script: []llm.Result{
		{Text: "好，我建个定时任务，建完告诉你。稍等。"},
		callTool("make_doc", `{"title":"x"}`),
		{Text: "建好了：每天 07:30。"},
	}}
	tg := &fileTG{}
	app := newPluginApp(t, chat, tg, []string{p.URL}, "tok", nil)
	app.discoverAll(context.Background())
	app.Handle(context.Background(), msg(42, "每天7:30给我晨报"))
	tg.mu.Lock()
	sent := append([]string(nil), tg.sent...)
	tg.mu.Unlock()
	if len(sent) != 1 || sent[0] != "建好了：每天 07:30。" {
		t.Fatalf("user must only see the real outcome: %q", sent)
	}
	if len(chat.Calls) != 3 || !strings.Contains(joinContents(chat.Calls[1]), "没有调用任何工具") {
		t.Fatalf("model was not nudged: %d calls", len(chat.Calls))
	}
	for _, m := range app.st.MessagesAfter(42, 0, 50) {
		if strings.Contains(m.Content, "稍等") || strings.Contains(m.Content, "没有调用任何工具") {
			t.Fatalf("announcement/nudge leaked into history: %q", m.Content)
		}
	}
}

func TestNudgeHappensAtMostOnceAndNotForReportsOrWithoutTools(t *testing.T) {
	for _, s := range []string{"我写了一份总结，放在上面。", "这个公式的推导很直接。", "我做的是加权平均。"} {
		if promisesAction(s) {
			t.Errorf("false positive: %q", s)
		}
	}
	for _, s := range []string{"好，我建个定时任务，稍等", "我这就去搜索一下", "做完告诉你", "完成后通知你"} {
		if !promisesAction(s) {
			t.Errorf("missed: %q", s)
		}
	}
	p := newFakePlugin(t, "tok", pluginTools)
	chat := &testutil.ToolChat{Script: []llm.Result{{Text: "稍等，我去搜索一下。"}}} // keeps promising
	tg := &fileTG{}
	app := newPluginApp(t, chat, tg, []string{p.URL}, "tok", nil)
	app.discoverAll(context.Background())
	app.Handle(context.Background(), msg(42, "查一下"))
	if len(chat.Calls) != 2 || tg.last() != "稍等，我去搜索一下。" {
		t.Fatalf("calls=%d last=%q (one nudge, then the reply goes out)", len(chat.Calls), tg.last())
	}
}
