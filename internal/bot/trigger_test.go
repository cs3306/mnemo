package bot

import (
	"companion/internal/telegram"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func triggerPost(t *testing.T, srv *httptest.Server, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/trigger", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTriggerRunsScheduledTurnAndStoresOnlyReply(t *testing.T) {
	srv, app, chat := apiApp(t)
	tg := app.tg.(*fakeTG)
	chat.Replies = []string{"早安！今天有三件事。"}
	if r := triggerPost(t, srv, "secret", `{"prompt":"查今天的新闻，给我晨报","label":"晨报"}`); r.StatusCode != 202 {
		t.Fatalf("status %d", r.StatusCode)
	}
	app.Wait()
	if got := tg.last(); got != "早安！今天有三件事。" {
		t.Fatalf("telegram got %q", got)
	}
	if len(chat.Calls) == 0 {
		t.Fatal("model not called")
	}
	var prompt string
	for _, m := range chat.Calls[0] {
		prompt += fmt.Sprint(m.Content) + "\n"
	}
	if !strings.Contains(prompt, "定时任务「晨报」") || !strings.Contains(prompt, "查今天的新闻，给我晨报") {
		t.Fatalf("instruction missing from prompt:\n%s", prompt)
	}
	var user, assistant int
	for _, m := range app.st.MessagesAfter(42, 0, 100) {
		switch m.Role {
		case "user":
			user++
		case "assistant":
			assistant++
		}
	}
	if user != 0 || assistant != 1 {
		t.Fatalf("stored user=%d assistant=%d, want 0/1", user, assistant)
	}
}

func TestTriggerValidation(t *testing.T) {
	srv, _, chat := apiApp(t)
	cases := []struct {
		key, body string
		want      int
	}{
		{"", `{"prompt":"x"}`, 401},
		{"bad", `{"prompt":"x"}`, 401},
		{"secret", `not json`, 400},
		{"secret", `{"prompt":"  "}`, 400},
		{"secret", `{"prompt":"` + strings.Repeat("字", triggerMaxPrompt+1) + `"}`, 400},
		{"secret", `{"prompt":"x","chat":7}`, 403},
	}
	for _, c := range cases {
		if r := triggerPost(t, srv, c.key, c.body); r.StatusCode != c.want {
			t.Errorf("%q -> %d, want %d", c.body[:min(len(c.body), 30)], r.StatusCode, c.want)
		}
	}
	if len(chat.Calls) != 0 {
		t.Fatal("rejected requests must not reach the model")
	}
}

func TestEmptyScheduledReplyIsReported(t *testing.T) {
	srv, app, chat := apiApp(t)
	tg := app.tg.(*fakeTG)
	chat.Replies = []string{"   "}
	triggerPost(t, srv, "secret", `{"prompt":"x","label":"空任务"}`)
	app.Wait()
	if !strings.Contains(tg.last(), "空任务") {
		t.Fatalf("user must hear about an empty result, got %q", tg.last())
	}
}

type menuTG struct {
	fakeTG
	chats []int64
	cmds  []telegram.BotCommand
}

func (m *menuTG) SetCommands(_ context.Context, chat int64, cmds []telegram.BotCommand) error {
	m.chats = append(m.chats, chat)
	m.cmds = cmds
	return nil
}

func TestCommandMenuIsPublishedForAllowedUsersOnly(t *testing.T) {
	app, _, _, _ := newApp(t, "42")
	m := &menuTG{}
	app.tg = m
	app.registerCommands(context.Background())
	if len(m.chats) != 1 || m.chats[0] != 42 {
		t.Fatalf("menu published for %v", m.chats)
	}
	seen := map[string]bool{}
	for _, c := range commandMenu {
		if seen[c.Command] || c.Command != strings.ToLower(c.Command) || len(c.Description) == 0 || len([]rune(c.Description)) > 256 {
			t.Errorf("bad menu entry %+v", c)
		}
		seen[c.Command] = true
		if !strings.Contains(help, "/"+c.Command) && c.Command != "help" {
			t.Errorf("/%s is in the menu but not in /help", c.Command)
		}
	}
	if len(commandMenu) > 100 {
		t.Fatal("Telegram allows at most 100 commands")
	}
}
