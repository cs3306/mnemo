package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"companion/internal/telegram"
	"companion/internal/testutil"
)

// filePlugin is a plugin server that takes files: code/body is what POST /v1/files answers.
type filePlugin struct {
	*httptest.Server
	mu   sync.Mutex
	got  []map[string]any
	code int
	body string
}

func newFilePlugin(t *testing.T, code int, body string) *filePlugin {
	p := &filePlugin{code: code, body: body}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tools":
			w.Write([]byte(`{"tools":[]}`))
		case r.Method == "POST" && r.URL.Path == "/v1/files":
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			p.mu.Lock()
			p.got = append(p.got, m)
			p.mu.Unlock()
			w.WriteHeader(p.code)
			w.Write([]byte(p.body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func docMsg(uid int64, caption, name, mime string, size int64) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{Chat: telegram.Chat{ID: uid, Type: "private"}, From: &telegram.User{ID: uid},
		Caption: caption, Document: &telegram.Document{FileID: "f1", FileName: name, MimeType: mime, FileSize: size}}}
}

func lastUserText(c *testutil.Chat) string {
	m := c.Calls[len(c.Calls)-1]
	return fmt.Sprint(m[len(m)-1].Content)
}

func TestDocumentIsForwardedToThePluginAndTheModelIsTold(t *testing.T) {
	p := newFilePlugin(t, 200, `{"text":"已保存到开发容器：/workspace/ai/inbox/proj.zip（3 B）"}`)
	chat := &testutil.Chat{Replies: []string{"收到，我来解压。"}}
	tg := &fakeTG{}
	app := newPluginApp(t, chat, tg, []string{p.URL}, "", nil)
	app.Handle(context.Background(), docMsg(42, "/整理一下", "proj.zip", "application/zip", 3))
	if len(p.got) != 1 || p.got[0]["name"] != "proj.zip" || p.got[0]["mime"] != "application/zip" {
		t.Fatalf("plugin got %v", p.got)
	}
	if p.got[0]["data"] != "aW1n" { // base64("img"), what fakeTG.Download returns
		t.Fatalf("data %v", p.got[0]["data"])
	}
	if c := p.got[0]["context"].(map[string]any); c["chat"] != float64(42) || c["channel"] != "telegram" {
		t.Fatalf("context %v", c)
	}
	if len(tg.sent) != 2 || !strings.HasPrefix(tg.sent[0], "📥 已保存到开发容器：/workspace/ai/inbox/proj.zip") || tg.sent[1] != "收到，我来解压。" {
		t.Fatalf("messages %q", tg.sent)
	}
	if u := lastUserText(chat); !strings.Contains(u, "/整理一下") || !strings.Contains(u, "「proj.zip」") || !strings.Contains(u, "/workspace/ai/inbox/proj.zip") {
		t.Fatalf("model saw %q", u) // a caption starting with / must not be taken for a bot command
	}
}

func TestDocumentWithoutCaptionStillReachesTheModel(t *testing.T) {
	p := newFilePlugin(t, 200, `{"text":"已保存：/w/inbox/a.txt"}`)
	chat := &testutil.Chat{Replies: []string{"要我怎么处理？"}}
	app := newPluginApp(t, chat, &fakeTG{}, []string{p.URL}, "", nil)
	app.Handle(context.Background(), docMsg(42, "", "a.txt", "text/plain", 3))
	if u := lastUserText(chat); !strings.Contains(u, "/w/inbox/a.txt") {
		t.Fatalf("model saw %q", u)
	}
}

func TestDocumentNobodyAcceptsAndRefusalsAreReportedWithoutCallingTheModel(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		body, want string
	}{
		{"no receiver", 404, `{"error":"no file receiver enabled"}`, "没有插件愿意接收"},
		{"refused", 400, `{"error":"文件超过 1 MB 上限"}`, "保存文件失败：文件超过 1 MB 上限"},
		{"bad token", 401, `{"error":"unauthorized"}`, "PLUGIN_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newFilePlugin(t, c.code, c.body)
			chat := &testutil.Chat{}
			tg := &fakeTG{}
			app := newPluginApp(t, chat, tg, []string{p.URL}, "", nil)
			app.Handle(context.Background(), docMsg(42, "看看", "a.bin", "application/octet-stream", 3))
			if len(chat.Calls) != 0 || len(tg.sent) != 1 || !strings.Contains(tg.sent[0], c.want) {
				t.Fatalf("model calls %d, messages %q", len(chat.Calls), tg.sent)
			}
		})
	}
}

func TestOversizedDocumentIsRefusedBeforeDownloading(t *testing.T) {
	p := newFilePlugin(t, 200, `{"text":"x"}`)
	chat := &testutil.Chat{}
	tg := &fakeTG{}
	app := newPluginApp(t, chat, tg, []string{p.URL}, "", nil)
	app.Handle(context.Background(), docMsg(42, "", "big.iso", "application/x-iso9660-image", 30<<20))
	if len(p.got) != 0 || len(chat.Calls) != 0 || len(tg.sent) != 1 || !strings.Contains(tg.sent[0], "20 MB") {
		t.Fatalf("plugin got %d, messages %q", len(p.got), tg.sent)
	}
}

func TestImageDocumentsStillGoToVisionNotThePlugin(t *testing.T) {
	p := newFilePlugin(t, 200, `{"text":"x"}`)
	app := newPluginApp(t, &testutil.Chat{}, &fakeTG{}, []string{p.URL}, "", nil) // no vision backend configured
	tg := app.tg.(*fakeTG)
	app.Handle(context.Background(), docMsg(42, "", "pic.png", "image/png", 3))
	if len(p.got) != 0 || len(tg.sent) != 1 || !strings.Contains(tg.sent[0], "读图") {
		t.Fatalf("plugin got %d, messages %q", len(p.got), tg.sent)
	}
}
