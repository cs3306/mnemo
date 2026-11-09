package llm

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
)

func TestCompleteRetriesAndStripsThink(t *testing.T) {
	RetryDelay = time.Millisecond
	var n int32
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if atomic.AddInt32(&n, 1) == 1 {
			http.Error(w, "busy", 429)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"choices":[{"message":{"content":"<think>x</think>你好"}}]}`))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m", Timeout: 5 * time.Second,
		ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}})
	got, err := c.Complete(context.Background(), []Message{Text("user", "hi")}, 0.5)
	if err != nil || got != "你好" || n != 2 {
		t.Fatalf("got %q err %v calls %d", got, err, n)
	}
	if body["chat_template_kwargs"] == nil || body["model"] != "m" {
		t.Fatalf("extra body not merged: %v", body)
	}
}

func TestNoRetryOn400(t *testing.T) {
	RetryDelay = time.Millisecond
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		http.Error(w, "bad", 400)
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL, Model: "m", Timeout: time.Second})
	if _, err := c.Complete(context.Background(), []Message{Text("user", "x")}, 0); err == nil || n != 1 {
		t.Fatalf("err=%v calls=%d", err, n)
	}
}

func TestEmbedBatchesAndNormalises(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&req)
		var sb strings.Builder
		sb.WriteString(`{"data":[`)
		for i := range req.Input {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"index":` + string(rune('0'+i%10)) + `,"embedding":[3,4]}`)
		}
		sb.WriteString(`]}`)
		w.Write([]byte(sb.String()))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL, Model: "e", Timeout: time.Second})
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = "t"
	}
	out, err := c.Embed(context.Background(), texts)
	if err != nil || len(out) != 40 || calls != 2 {
		t.Fatalf("err=%v len=%d calls=%d", err, len(out), calls)
	}
	if out[0][0] < 0.59 || out[0][0] > 0.61 {
		t.Fatalf("not normalised: %v", out[0])
	}
}

func TestUsageAndToolCallsParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["tools"] == nil {
			t.Error("tools not forwarded")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"想","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}}],
		 "usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40,"completion_tokens_details":{"reasoning_tokens":3}}}`))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL, Model: "m", Timeout: time.Second})
	var got Usage
	c.OnUsage = func(u Usage) { got = u }
	r, err := c.CompleteTools(context.Background(), []Message{Text("user", "x")}, json.RawMessage(`[{"type":"function"}]`), 0)
	if err != nil || len(r.ToolCalls) == 0 || r.Reasoning != "想" {
		t.Fatalf("%+v %v", r, err)
	}
	if got != (Usage{Hit: 60, Miss: 40, Out: 7, Reasoning: 3}) {
		t.Fatalf("usage %+v", got)
	}
}

func TestUsageWithoutCacheFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"hi","tool_calls":[]}}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL, Model: "m", Timeout: time.Second})
	var got Usage
	c.OnUsage = func(u Usage) { got = u }
	r, _ := c.CompleteTools(context.Background(), nil, nil, 0)
	if got.Miss != 100 || got.Hit != 0 || len(r.ToolCalls) != 0 {
		t.Fatalf("%+v %+v", got, r)
	}
}

func TestBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %s", r.URL.Path)
		}
		w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	s, err := c.Balance(context.Background())
	if err != nil || !strings.Contains(s, "CNY 110.00") {
		t.Fatalf("%q %v", s, err)
	}
}

// Strict chat templates (Qwen3 on llama-server --jinja) answer 500 "System message must be at the beginning" when the
// tool loop appends a system notice. The client must resend it as a user message, and keep doing so afterwards.
func TestMidSystemMessageDemotedAfterTemplateError(t *testing.T) {
	RetryDelay = time.Millisecond
	var n int32
	var lastRoles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		lastRoles = nil
		for i, m := range body.Messages {
			lastRoles = append(lastRoles, m.Role)
			if m.Role == "system" && i > 0 {
				http.Error(w, `{"error":{"code":500,"message":"Error: Jinja Exception: System message must be at the beginning."}}`, 500)
				return
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL, Model: "m", Timeout: 5 * time.Second})
	msgs := []Message{Text("system", "persona"), Text("user", "hi"), Text("system", "notice")}
	got, err := c.Complete(context.Background(), msgs, 0)
	if err != nil || got != "ok" {
		t.Fatalf("got %q err %v", got, err)
	}
	if n != 2 || strings.Join(lastRoles, ",") != "system,user,user" {
		t.Fatalf("calls=%d roles=%v", n, lastRoles)
	}
	if msgs[2].Role != "system" {
		t.Fatal("caller's slice was modified")
	}
	n = 0
	if _, err := c.Complete(context.Background(), msgs, 0); err != nil || n != 1 {
		t.Fatalf("second call should demote up front: calls=%d err=%v", n, err)
	}
}

func TestLeadingSystemUntouched(t *testing.T) {
	out, changed := demoteMidSystem([]Message{Text("system", "a"), Text("user", "b")})
	if changed || out[0].Role != "system" {
		t.Fatalf("changed=%v %v", changed, out)
	}
}
