package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/store"
	"companion/internal/telegram"
	"companion/internal/testutil"
)

type fakeTG struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeTG) Updates(ctx context.Context, _ int64, _ int) ([]telegram.Update, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *fakeTG) Send(_ context.Context, _ int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeTG) Typing(context.Context, int64)                    {}
func (f *fakeTG) Download(context.Context, string) ([]byte, error) { return []byte("img"), nil }
func (f *fakeTG) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

func newApp(t *testing.T, allowed string) (*App, *fakeTG, *testutil.Chat, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": dir, "ALLOWED_USER_IDS": allowed, "EXTRACT_EVERY": "1", "LLM_MODEL": "chat-m"}[k]
	})
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := &fakeTG{}
	chat := &testutil.Chat{Replies: []string{"早呀，今天想学什么？"}}
	maint := &testutil.Chat{Replies: []string{`{"memories":[{"kind":"fact","text":"用户周五要交线代作业","importance":4}],"learning":[]}`}}
	app, err := New(cfg, Deps{Store: st, Chat: chat, Maint: maint, Embed: &testutil.Embed{Name: "e"}, TG: tg, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return app, tg, chat, st
}

func msg(uid int64, text string) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{Chat: telegram.Chat{ID: uid, Type: "private"}, From: &telegram.User{ID: uid}, Text: text}}
}

func TestWhitelist(t *testing.T) {
	app, tg, chat, _ := newApp(t, "42")
	app.Handle(context.Background(), msg(99, "你好"))
	if len(tg.sent) != 0 || len(chat.Calls) != 0 {
		t.Fatal("stranger must be ignored silently")
	}
	app2, tg2, _, _ := newApp(t, "")
	app2.Handle(context.Background(), msg(99, "你好"))
	if !strings.Contains(tg2.last(), "99") {
		t.Fatal("empty whitelist should reveal the user's ID")
	}
}

func TestTurnExtractsMemoryAndCommandsWork(t *testing.T) {
	app, tg, chat, st := newApp(t, "42")
	ctx := context.Background()
	app.Handle(ctx, msg(42, "我周五要交线代作业"))
	app.Wait()
	if tg.sent[0] != "早呀，今天想学什么？" || len(chat.Calls) != 1 {
		t.Fatalf("reply not sent: %v", tg.sent)
	}
	if len(st.ActiveMemories()) != 1 {
		t.Fatal("background extraction did not store a memory")
	}
	app.Handle(ctx, msg(42, "/memory 线代"))
	if !strings.Contains(tg.last(), "线代作业") {
		t.Fatalf("/memory: %q", tg.last())
	}
	app.Handle(ctx, msg(42, "/pin"))
	app.Handle(ctx, msg(42, "/pins"))
	if !strings.Contains(tg.last(), "早呀") {
		t.Fatalf("/pins: %q", tg.last())
	}
	id := st.ActiveMemories()[0].ID
	app.Handle(ctx, msg(42, "/edit "+itoa(id)+" 用户周六要交作业"))
	app.Handle(ctx, msg(42, "/forget "+itoa(id)))
	if len(st.ActiveMemories()) != 0 {
		t.Fatal("/forget failed")
	}
	app.Handle(ctx, msg(42, "/status"))
	if !strings.Contains(tg.last(), "chat-m") {
		t.Fatalf("/status: %q", tg.last())
	}
	// the next turn's prompt must not contain the deleted memory
	app.Handle(ctx, msg(42, "继续"))
	app.Wait()
	last := chat.Calls[len(chat.Calls)-1]
	if strings.Contains(strings.Join(func() []string {
		var s []string
		for _, m := range last {
			if x, ok := m.Content.(string); ok {
				s = append(s, x)
			}
		}
		return s
	}(), "\n"), "周六要交作业") {
		t.Fatal("deleted memory leaked into prompt")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
