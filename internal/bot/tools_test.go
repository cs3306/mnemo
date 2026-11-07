package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/store"
	"companion/internal/testutil"
)

func newToolApp(t *testing.T, chat llm.Completer, vision llm.Completer, mut func(*config.Config)) (*App, *httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": dir, "ALLOWED_USER_IDS": "42", "EXTRACT_EVERY": "100", "LLM_MODEL": "m", "API_KEY": "secret"}[k]
	})
	if mut != nil {
		mut(cfg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app, err := New(cfg, Deps{Store: st, Chat: chat, Maint: &testutil.Chat{}, Vision: vision, Embed: &testutil.Embed{Name: "e"}, TG: &fakeTG{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	app.cfg.APIKey = "secret"
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return app, srv, st
}

func decode(t *testing.T, resp *http.Response) (finish string, msg map[string]any) {
	t.Helper()
	var out struct {
		Choices []struct {
			FinishReason string         `json:"finish_reason"`
			Message      map[string]any `json:"message"`
		}
		Error any
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Choices) == 0 {
		t.Fatalf("no choices, status %d, error %v", resp.StatusCode, out.Error)
	}
	return out.Choices[0].FinishReason, out.Choices[0].Message
}

const clientTool = `[{"type":"function","function":{"name":"capture_screen","description":"d","parameters":{"type":"object","properties":{}}}}]`
const callJSON = `[{"id":"c1","type":"function","function":{"name":"capture_screen","arguments":"{}"}}]`

func TestClientToolRoundTripWithImageResult(t *testing.T) {
	chat := &testutil.ToolChat{Script: []llm.Result{
		{ToolCalls: json.RawMessage(callJSON)},
		{Text: "我看到了函数图像。"},
	}}
	vision := &testutil.Chat{Replies: []string{"转写：y=x^2 的图像"}}
	app, srv, st := newToolApp(t, chat, vision, nil)

	// request 1: the model asks the client to capture the screen
	resp := post(t, srv, "secret", `{"tools":`+clientTool+`,"messages":[{"role":"user","content":"帮我看看屏幕"}]}`)
	finish, msg := decode(t, resp)
	if finish != "tool_calls" || msg["tool_calls"] == nil {
		t.Fatalf("expected tool_calls, got %q %v", finish, msg)
	}
	if n := len(st.Unsummarized(42)); n != 1 {
		t.Fatalf("only the user message may be stored while the turn is open, got %d", n)
	}

	// request 2: the client returns the screenshot inside the tool result
	body := `{"tools":` + clientTool + `,"messages":[
	 {"role":"user","content":"帮我看看屏幕"},
	 {"role":"assistant","content":null,"tool_calls":` + callJSON + `},
	 {"role":"tool","tool_call_id":"c1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`
	finish, msg = decode(t, post(t, srv, "secret", body))
	if finish != "stop" || msg["content"] != "我看到了函数图像。" {
		t.Fatalf("%q %v", finish, msg)
	}
	app.Wait()
	rows := st.Unsummarized(42)
	if len(rows) != 2 || rows[0].Role != "user" || rows[1].Role != "assistant" {
		t.Fatalf("history must be exactly user+assistant (no tool noise, no duplicate user): %+v", rows)
	}
	second := chat.Calls[1]
	tail := second[len(second)-2:]
	if tail[0].Role != "assistant" || len(tail[0].ToolCalls) == 0 || tail[1].Role != "tool" || tail[1].ToolCallID != "c1" ||
		!strings.Contains(tail[1].Content.(string), "y=x^2") {
		t.Fatalf("tool exchange not forwarded to the model with the transcribed image: %+v", tail)
	}
	last := 0
	for i, m := range second {
		if m.Role == "user" {
			last = i
		}
	}
	if second[last].Content == nil || strings.Count(joinContents(second), "帮我看看屏幕") != 1 {
		t.Fatal("user message must appear exactly once in the continued prompt")
	}
}

func joinContents(ms []llm.Message) string {
	var s []string
	for _, m := range ms {
		if x, ok := m.Content.(string); ok {
			s = append(s, x)
		}
	}
	return strings.Join(s, "\n")
}

func TestBuiltinSearchRunsServerSide(t *testing.T) {
	var searches int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/search" || r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("bad search request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		atomic.AddInt32(&searches, 1)
		w.Write([]byte(`{"results":[{"title":"Example 招生","url":"https://example.com/x","summary":"摘要内容","site_name":"Example","date":"2026-10-01"}]}`))
	}))
	defer search.Close()
	call := `[{"id":"s1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"Example\"}"}}]`
	chat := &testutil.ToolChat{Script: []llm.Result{{ToolCalls: json.RawMessage(call)}, {Text: "查到了。"}}}
	app, srv, st := newToolApp(t, chat, nil, func(c *config.Config) {
		c.SearchEnabled, c.SearchBase, c.SearchKey, c.MaxToolRounds = true, search.URL, "sk", 4
	})
	finish, msg := decode(t, post(t, srv, "secret", `{"messages":[{"role":"user","content":"查一下 Example"}]}`))
	if finish != "stop" || msg["content"] != "查到了。" || searches != 1 {
		t.Fatalf("%q %v searches=%d", finish, msg, searches)
	}
	if !strings.Contains(joinContents(chat.Calls[1]), "摘要内容") {
		t.Fatal("search results were not given to the model")
	}
	if !strings.Contains(string(chat.ToolsSeen[0]), "web_search") {
		t.Fatal("built-in tool definitions not offered to the model")
	}
	app.Wait()
	if len(st.Unsummarized(42)) != 2 {
		t.Fatal("search exchange must not be stored in history")
	}
}

func TestToolLoopIsBounded(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"results":[]}`)) }))
	defer search.Close()
	call := `[{"id":"s1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]`
	chat := &testutil.ToolChat{Script: []llm.Result{{ToolCalls: json.RawMessage(call)}}} // never stops
	_, srv, _ := newToolApp(t, chat, nil, func(c *config.Config) {
		c.SearchEnabled, c.SearchBase, c.MaxToolRounds = true, search.URL, 2
	})
	resp := post(t, srv, "secret", `{"messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 502 || len(chat.Calls) != 3 {
		t.Fatalf("status %d, backend calls %d (want 502 after 2 rounds + 1 forced-final attempt)", resp.StatusCode, len(chat.Calls))
	}
	if last := chat.Calls[2]; last[len(last)-1].Role != "system" {
		t.Fatal("the final attempt must tell the model to stop calling tools")
	}
}

func TestToolsRejectedWhenBackendCannotDoThem(t *testing.T) {
	_, srv, _ := newToolApp(t, &testutil.Chat{}, nil, nil)
	resp := post(t, srv, "secret", `{"tools":`+clientTool+`,"messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 502 && resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStatusShowsCacheHitRate(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"好"}}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_cache_hit_tokens":750,"prompt_cache_miss_tokens":250,"completion_tokens_details":{"reasoning_tokens":0}}}`))
	}))
	defer llmSrv.Close()
	chat := llm.New(config.Endpoint{BaseURL: llmSrv.URL, Model: "m", Timeout: time.Second})
	app, _, _ := newToolApp(t, chat, nil, func(c *config.Config) { c.PriceHit, c.PriceMiss, c.PriceOut = 0.15, 4.5, 13.5 })
	ctx := context.Background()
	app.Handle(ctx, msg(42, "你好"))
	app.Wait()
	tg := app.tg.(*fakeTG)
	app.Handle(ctx, msg(42, "/status"))
	out := tg.last()
	if !strings.Contains(out, "缓存命中 75%") || !strings.Contains(out, "输出 50") || !strings.Contains(out, "元") {
		t.Fatalf("/status: %q", out)
	}
}

// Cache friendliness: every turn's prompt must start with the previous turn's prompt (minus its volatile last message).
func TestPromptPrefixIsStableAcrossTurns(t *testing.T) {
	chat := &testutil.Chat{Replies: []string{"回复一", "回复二", "回复三", "回复四"}}
	app, _, _ := newToolApp(t, chat, nil, nil)
	ctx := context.Background()
	for _, m := range []string{"第一条消息内容", "第二条消息内容", "第三条消息内容", "第四条消息内容"} {
		app.Handle(ctx, msg(42, m))
	}
	for n := 1; n < len(chat.Calls); n++ {
		prev, cur := chat.Calls[n-1], chat.Calls[n]
		k := len(prev) - 1 // the last message of the previous prompt carried per-turn memories/time
		for i := 0; i < k; i++ {
			if prev[i].Role != cur[i].Role || prev[i].Content != cur[i].Content {
				t.Fatalf("turn %d: prompt prefix changed at message %d", n+1, i)
			}
		}
	}
}
