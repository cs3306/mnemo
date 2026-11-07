package bot

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/testutil"
)

func TestSearxngSearchRunsServerSide(t *testing.T) {
	var hits int32
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/search" || r.URL.Query().Get("q") != "线性代数 特征值" || r.URL.Query().Get("format") != "json" {
			t.Errorf("bad request %s", r.URL.String())
		}
		w.Write([]byte(`{"results":[{"title":"特征值入门","url":"https://example.org/a","content":"摘要：特征值是……","engine":"duckduckgo","publishedDate":"2026-10-01"}]}`))
	}))
	defer sx.Close()
	chat := &testutil.ToolChat{Script: []llm.Result{{ToolCalls: json.RawMessage(`[{"id":"s1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"线性代数 特征值\"}"}}]`)}, {Text: "查到了。"}}}
	_, srv, _ := newToolApp(t, chat, nil, func(c *config.Config) {
		c.SearchEnabled, c.SearchProvider, c.SearxngURL, c.MaxToolRounds = true, "searxng", sx.URL, 4
	})
	finish, msg := decode(t, post(t, srv, "secret", `{"messages":[{"role":"user","content":"查一下"}]}`))
	if finish != "stop" || msg["content"] != "查到了。" || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("%q %v hits=%d", finish, msg, hits)
	}
	if got := joinContents(chat.Calls[1]); !strings.Contains(got, "特征值入门") || !strings.Contains(got, "https://example.org/a") {
		t.Fatalf("results not given to the model: %s", got)
	}
}

func TestSearxngErrorsAreReadable(t *testing.T) {
	for code, want := range map[int]string{403: "json 加入 search.formats", 429: "限流", 500: "HTTP 500"} {
		code, want := code, want
		sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		app := toolsApp(t, func(c *config.Config) { c.SearchEnabled, c.SearchProvider, c.SearxngURL = true, "searxng", sx.URL })
		out := runTool(app, "web_search", `{"query":"x"}`)
		sx.Close()
		if !strings.Contains(out, "搜索失败") || !strings.Contains(out, want) {
			t.Fatalf("HTTP %d -> %q", code, out)
		}
	}
	// not JSON at all
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }))
	defer sx.Close()
	app := toolsApp(t, func(c *config.Config) { c.SearchEnabled, c.SearchProvider, c.SearxngURL = true, "searxng", sx.URL })
	if out := runTool(app, "web_search", `{"query":"x"}`); !strings.Contains(out, "不是 JSON") {
		t.Fatalf("%q", out)
	}
}

func TestSearxngEmptyResults(t *testing.T) {
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"results":[]}`)) }))
	defer sx.Close()
	app := toolsApp(t, func(c *config.Config) { c.SearchEnabled, c.SearchProvider, c.SearxngURL = true, "searxng", sx.URL })
	if out := runTool(app, "web_search", `{"query":"x"}`); out != "没有搜索结果" {
		t.Fatalf("%q", out)
	}
}

const page = `<!doctype html><html><head><title>特征值 &amp; 特征向量</title><style>.x{color:red}</style>
<script>var secret = "SCRIPT-BODY";</script></head><body><!-- hidden comment -->
<h1>标题</h1><p>第一段&nbsp;内容 &lt;b&gt;</p><div>第二段<br>换行</div><noscript>NOSCRIPT-BODY</noscript></body></html>`

func TestFetchConvertsHTMLToText(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer site.Close()
	app := toolsApp(t, func(c *config.Config) {
		c.SearchEnabled, c.SearchProvider, c.SearxngURL, c.FetchAllowPrivate = true, "searxng", "http://x", true
	})
	out := runTool(app, "web_fetch", `{"url":"`+site.URL+`/p"}`)
	for _, want := range []string{"特征值 & 特征向量", "标题", "第一段 内容 <b>", "第二段", "换行"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	for _, bad := range []string{"SCRIPT-BODY", "NOSCRIPT-BODY", "hidden comment", "color:red", "<h1>"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q must not appear in %q", bad, out)
		}
	}
}

func TestFetchLimitsAndTypes(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(strings.Repeat("字", 3_000_000)))
		case "/bin":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF"))
		case "/404":
			http.NotFound(w, r)
		case "/redir":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/ok":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("plain text"))
		}
	}))
	defer site.Close()
	app := toolsApp(t, func(c *config.Config) {
		c.SearchEnabled, c.SearchProvider, c.SearxngURL, c.FetchAllowPrivate = true, "searxng", "http://x", true
	})
	if out := runTool(app, "web_fetch", `{"url":"`+site.URL+`/big"}`); len([]rune(out)) > 6001 || !strings.HasPrefix(out, "字") {
		t.Fatalf("a huge page must be cut to 6000 runes, got %d", len([]rune(out)))
	}
	if out := runTool(app, "web_fetch", `{"url":"`+site.URL+`/bin"}`); !strings.Contains(out, "不支持的内容类型") {
		t.Fatalf("%q", out)
	}
	if out := runTool(app, "web_fetch", `{"url":"`+site.URL+`/404"}`); !strings.Contains(out, "HTTP 404") {
		t.Fatalf("%q", out)
	}
	if out := runTool(app, "web_fetch", `{"url":"`+site.URL+`/redir"}`); out != "plain text" {
		t.Fatalf("redirects should be followed, got %q", out)
	}
}

func TestFetchRefusesInternalAddresses(t *testing.T) {
	hit := int32(0)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hit, 1) }))
	defer local.Close()
	app := toolsApp(t, func(c *config.Config) { c.SearchEnabled, c.SearchProvider, c.SearxngURL = true, "searxng", "http://x" })
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(local.URL, "http://"))
	for _, u := range []string{
		local.URL, "http://localhost:" + port + "/", "http://[::1]:" + port + "/", "http://0.0.0.0:" + port + "/",
		"http://169.254.169.254/latest/meta-data/", "http://10.0.0.5/", "http://192.168.1.1/", "http://172.16.0.1/",
		"http://100.64.0.1/", "http://[::ffff:127.0.0.1]:" + port + "/", "http://[fd00::1]/", "http://[fe80::1]/",
	} {
		out := runTool(app, "web_fetch", `{"url":"`+u+`"}`)
		if !strings.Contains(out, "抓取失败") || !strings.Contains(out, "不允许访问内网") {
			t.Errorf("%s was not refused: %q", u, out)
		}
	}
	if atomic.LoadInt32(&hit) != 0 {
		t.Fatal("the internal server must never be contacted")
	}
	// scheme / credentials are rejected before any network access
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "http://user:pw@example.com/", "javascript:alert(1)"} {
		if out := runTool(app, "web_fetch", `{"url":"`+u+`"}`); !strings.Contains(out, "http(s)") {
			t.Errorf("%s: %q", u, out)
		}
	}
}

func TestFetchRedirectsAreChecked(t *testing.T) {
	app := toolsApp(t, func(c *config.Config) { c.SearchEnabled, c.SearchProvider, c.SearxngURL = true, "searxng", "http://x" })
	mk := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		return &http.Request{URL: u}
	}
	if err := app.fetchc.CheckRedirect(mk("http://10.1.2.3/"), nil); err == nil {
		t.Fatal("redirect to a private address must be refused")
	}
	if err := app.fetchc.CheckRedirect(mk("http://[::1]/"), nil); err == nil {
		t.Fatal("redirect to loopback must be refused")
	}
	if err := app.fetchc.CheckRedirect(mk("file:///etc/passwd"), nil); err == nil {
		t.Fatal("redirect to a non-http scheme must be refused")
	}
	if err := app.fetchc.CheckRedirect(mk("http://93.184.216.34/"), make([]*http.Request, fetchMaxRedirects)); err == nil {
		t.Fatal("too many redirects must be refused")
	}
	if err := app.fetchc.CheckRedirect(mk("http://93.184.216.34/"), nil); err != nil {
		t.Fatalf("a public address must pass: %v", err)
	}
}

func TestGuardedDialRefusesBlockedAddresses(t *testing.T) {
	d := guardedDial(&net.Dialer{})
	for _, addr := range []string{"127.0.0.1:80", "localhost:80", "[::1]:80", "10.0.0.1:80", "169.254.169.254:80"} {
		if _, err := d(context.Background(), "tcp", addr); err == nil || !strings.Contains(err.Error(), "不允许访问内网") {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestBlockedAddrTable(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.1.1": true, "172.31.0.1": true, "192.168.0.1": true, "169.254.1.1": true,
		"100.100.100.100": true, "0.0.0.0": true, "224.0.0.1": true, "fd12::1": true, "fe80::1": true, "::ffff:10.0.0.1": true,
		"8.8.8.8": false, "93.184.216.34": false, "198.18.3.26": false, "100.63.255.255": false, "2606:4700::1111": false,
	} {
		if got := blockedAddr(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: blocked=%v want %v", ip, got, want)
		}
	}
}

func TestHTMLToTextDirect(t *testing.T) {
	title, text := htmlToText(`<html><head><title> A  B </title></head><body><ul><li>one</li><li>two</li></ul>x<br/>y</body></html>`)
	if title != "A B" || text != "one\ntwo\nx\ny" && !strings.Contains(text, "one") {
		t.Fatalf("%q / %q", title, text)
	}
	if strings.Contains(text, "<") {
		t.Fatalf("tags left in %q", text)
	}
}

// helpers

func toolsApp(t *testing.T, mut func(*config.Config)) *App {
	t.Helper()
	app, _, _ := newToolApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "ok"}}}, nil, mut)
	return app
}

func runTool(app *App, name, args string) string {
	var c toolCall
	c.ID = "t"
	c.Function.Name, c.Function.Arguments = name, args
	return app.runBuiltin(context.Background(), c)
}
