package bot

import (
	"bufio"
	"companion/internal/testutil"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func apiApp(t *testing.T) (*httptest.Server, *App, *testutil.Chat) {
	t.Helper()
	app, _, chat, _ := newApp(t, "42")
	app.cfg.APIKey = "secret"
	app.cfg.APIModel = "companion"
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv, app, chat
}

func post(t *testing.T, srv *httptest.Server, key, body string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAPIAuthAndModels(t *testing.T) {
	srv, _, _ := apiApp(t)
	if r := post(t, srv, "wrong", `{"messages":[{"role":"user","content":"hi"}]}`); r.StatusCode != 401 {
		t.Fatalf("status %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, _ := http.DefaultClient.Do(req)
	var m struct{ Data []struct{ ID string } }
	json.NewDecoder(resp.Body).Decode(&m)
	if len(m.Data) != 2 || m.Data[0].ID != "companion" || m.Data[1].ID != "companion-raw" {
		t.Fatalf("%+v", m)
	}
}

func TestAPIMemoryChatUsesOnlyLastUserMessageAndSharesHistory(t *testing.T) {
	srv, app, chat := apiApp(t)
	body := `{"model":"companion","messages":[{"role":"system","content":"FRONTEND-SYSTEM"},{"role":"user","content":"旧历史"},{"role":"assistant","content":"旧回复"},{"role":"user","content":"我周五要交作业"}]}`
	resp := post(t, srv, "secret", body)
	var out struct {
		Choices []struct{ Message struct{ Content string } }
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out.Choices[0].Message.Content != "早呀，今天想学什么？" {
		t.Fatalf("status %d %+v", resp.StatusCode, out)
	}
	sent := ""
	for _, m := range chat.Calls[0] {
		if s, ok := m.Content.(string); ok {
			sent += s
		}
	}
	if strings.Contains(sent, "FRONTEND-SYSTEM") || strings.Contains(sent, "旧历史") || !strings.Contains(sent, "我周五要交作业") {
		t.Fatal("client-supplied history/system prompt must be ignored; last user message must be used")
	}
	app.Wait()
	// default conversation == Telegram user 42's conversation, so memory/history is shared
	if len(app.st.Unsummarized(42)) != 2 {
		t.Fatalf("API turn not stored in the shared default chat")
	}
	// a different X-Companion-Chat is isolated
	post(t, srv, "secret", `{"messages":[{"role":"user","content":"别的会话"}]}`, "X-Companion-Chat", "alice")
	app.Wait()
	if len(app.st.Unsummarized(42)) != 2 {
		t.Fatal("separate chat leaked into default")
	}
}

func TestAPIStreamingFormat(t *testing.T) {
	srv, app, _ := apiApp(t)
	resp := post(t, srv, "secret", `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatal(ct)
	}
	var text string
	sawStop, sawDone := false, false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		d, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if d == "[DONE]" {
			sawDone = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta        struct{ Content string }
				FinishReason *string `json:"finish_reason"`
			}
		}
		if err := json.Unmarshal([]byte(d), &c); err != nil {
			t.Fatal(err)
		}
		text += c.Choices[0].Delta.Content
		if c.Choices[0].FinishReason != nil && *c.Choices[0].FinishReason == "stop" {
			sawStop = true
		}
	}
	app.Wait()
	if text != "早呀，今天想学什么？" || !sawStop || !sawDone {
		t.Fatalf("text=%q stop=%v done=%v", text, sawStop, sawDone)
	}
}

func TestAPIRawModelBypassesMemory(t *testing.T) {
	srv, app, chat := apiApp(t)
	resp := post(t, srv, "secret", `{"model":"companion-raw","messages":[{"role":"system","content":"生成标题"},{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 200 || len(chat.Calls) != 1 || len(chat.Calls[0]) != 2 {
		t.Fatalf("raw must forward the client's messages untouched: %d %v", resp.StatusCode, chat.Calls)
	}
	if len(app.st.Unsummarized(42)) != 0 || len(app.st.ActiveMemories()) != 0 {
		t.Fatal("raw requests must not be stored")
	}
}

func TestAPIImageRules(t *testing.T) {
	srv, _, _ := apiApp(t)
	// remote URL is refused (no server-side fetching)
	r := post(t, srv, "secret", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://169.254.169.254/x"}}]}]}`)
	if r.StatusCode != 400 {
		t.Fatalf("remote image url must be rejected, got %d", r.StatusCode)
	}
	// vision disabled in test app -> clear 400
	r = post(t, srv, "secret", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`)
	if r.StatusCode != 400 {
		t.Fatalf("got %d", r.StatusCode)
	}
}
