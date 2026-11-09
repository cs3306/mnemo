package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestPrepareRich(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"keeps inline and block math",
			"群阶 $|E(\\mathbb{F}_p)|$ 如下\n\n$$\\phi: E \\to E'$$\n",
			"群阶 $|E(\\mathbb{F}_p)|$ 如下\n\n$$\\phi: E \\to E'$$\n"},
		{"escapes prices", "它花了 $5 和 $10。", "它花了 \\$5 和 \\$10。"},
		{"escapes a price glued to Chinese", "花了$5，赚了$10", "花了\\$5，赚了\\$10"},
		{"price then formula on one line", "价格 $5.00,$x$", "价格 \\$5.00,$x$"},
		{"two formulas", "$x$ 和 $y^2$", "$x$ 和 $y^2$"},
		{"already escaped stays", "金额 \\$950", "金额 \\$950"},
		{"code span untouched", "用 `echo $HOME` 看", "用 `echo $HOME` 看"},
		{"fenced code untouched", "```sh\necho $5\nx\ny\n```", "```sh\necho $5\nx\ny\n```"},
		{"unclosed display math escaped", "a $$ b", "a \\$\\$ b"},
		{"table after paragraph gets a blank line",
			"对比如下\n| a | b |\n| --- | --- |\n| 1 | 2 |",
			"对比如下\n\n| a | b |\n| --- | --- |\n| 1 | 2 |"},
		{"table after blank line unchanged", "x\n\n| a |\n| - |", "x\n\n| a |\n| - |"},
		{"soft breaks become paragraphs", "第一行\n第二行\n第三行", "第一行\n\n第二行\n\n第三行"},
		{"list stays tight", "- 一\n  - 二\n- 三", "- 一\n  - 二\n- 三"},
		{"paragraph then list", "先说结论\n- 一\n- 二", "先说结论\n- 一\n- 二"},
		{"heading and quote untouched", "# 标题\n正文\n> 引用", "# 标题\n正文\n> 引用"},
		{"multi-line math block untouched", "前\n$$\na\nb\n$$\n后", "前\n\n$$\na\nb\n$$\n后"},
		{"fence after paragraph gets a blank line", "看代码\n```go\nx\n```", "看代码\n\n```go\nx\n```"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PrepareRich(c.in); got != c.want {
				t.Errorf("PrepareRich(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
			if again := PrepareRich(c.want); again != c.want {
				t.Errorf("not idempotent on its own output: %q -> %q", c.want, again)
			}
		})
	}
}

type recorded struct {
	method string
	body   map[string]any
}

func fakeServer(t *testing.T, respond func(method string) (int, string)) (*httptest.Server, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var rec []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		rec = append(rec, recorded{m, body})
		mu.Unlock()
		code, out := respond(m)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return srv, &rec
}

func TestSendRichUsesRichMessageWithPreparedMarkdown(t *testing.T) {
	srv, rec := fakeServer(t, func(string) (int, string) { return 200, `{"ok":true,"result":{}}` })
	c := New("1:T", srv.URL)
	if err := c.SendRich(context.Background(), 42, "花了 $5\n第二行 $x^2$"); err != nil {
		t.Fatal(err)
	}
	if len(*rec) != 1 || (*rec)[0].method != "sendRichMessage" {
		t.Fatalf("%v", *rec)
	}
	rm := (*rec)[0].body["rich_message"].(map[string]any)
	if rm["markdown"] != "花了 \\$5\n\n第二行 $x^2$" || (*rec)[0].body["chat_id"].(float64) != 42 {
		t.Fatalf("%v", (*rec)[0].body)
	}
}

func TestSendRichFallsBackToPlainWhenRejected(t *testing.T) {
	srv, rec := fakeServer(t, func(m string) (int, string) {
		if m == "sendRichMessage" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message"}`
		}
		return 200, `{"ok":true,"result":{}}`
	})
	c := New("1:T", srv.URL)
	if err := c.SendRich(context.Background(), 1, "原文 $5"); err != nil {
		t.Fatal(err)
	}
	if len(*rec) != 2 || (*rec)[1].method != "sendMessage" || (*rec)[1].body["text"] != "原文 $5" {
		t.Fatalf("fallback must send the original text: %v", *rec)
	}
	if c.richOff.Load() {
		t.Fatal("a 400 must not disable rich messages")
	}
}

func TestSendRichStopsTryingAfter404(t *testing.T) {
	srv, rec := fakeServer(t, func(m string) (int, string) {
		if m == "sendRichMessage" {
			return 404, `{"ok":false,"error_code":404,"description":"Not Found"}`
		}
		return 200, `{"ok":true,"result":{}}`
	})
	c := New("1:T", srv.URL)
	for i := 0; i < 2; i++ {
		if err := c.SendRich(context.Background(), 1, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	var methods []string
	for _, r := range *rec {
		methods = append(methods, r.method)
	}
	if got := strings.Join(methods, ","); got != "sendRichMessage,sendMessage,sendMessage" {
		t.Fatalf("second reply must skip the rich attempt: %s", got)
	}
}

func TestSendRichDoesNotResendAfterNetworkError(t *testing.T) {
	srv, rec := fakeServer(t, func(string) (int, string) { return 200, `{"ok":true,"result":{}}` })
	url := srv.URL
	srv.Close()
	c := New("1:SECRET", url)
	err := c.SendRich(context.Background(), 1, "hi")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("want a token-free network error, got %v", err)
	}
	if len(*rec) != 0 {
		t.Fatalf("%v", *rec)
	}
}
