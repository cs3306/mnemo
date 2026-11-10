package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/testutil"
)

// eventSink is a plugin server that declares which events it wants and records what it receives.
type eventSink struct {
	*httptest.Server
	mu     sync.Mutex
	events []map[string]any
	auth   []string
}

func newEventSink(t *testing.T, status int, declares ...string) *eventSink {
	s := &eventSink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tools":
			json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "events": declares})
		case r.Method == "POST" && r.URL.Path == "/v1/events":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.events, s.auth = append(s.events, body), append(s.auth, r.Header.Get("Authorization"))
			s.mu.Unlock()
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *eventSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.events) }

func (s *eventSink) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		out = append(out, e["type"].(string))
	}
	return out
}

func (s *eventSink) first(typ string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e["type"] == typ {
			return e
		}
	}
	return nil
}

// eventApp builds an app whose operator allows `allow` (PLUGIN_EVENTS) and has discovered the given plugins.
func eventApp(t *testing.T, allow string, urls ...string) *App {
	app := newPluginApp(t, &testutil.ToolChat{Script: []llm.Result{{Text: "好"}, {Text: "好"}, {Text: "好"}}}, &fakeTG{}, urls, "tok",
		func(c *config.Config) {
			c.PluginEvents = map[string]bool{}
			switch allow {
			case "":
			case "all":
				for _, e := range config.EventTypes {
					c.PluginEvents[e] = true
				}
			default:
				c.PluginEvents[allow] = true
			}
		})
	app.discoverAll(context.Background())
	return app
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPluginEventsAreOffByDefault(t *testing.T) {
	s := newEventSink(t, 204, "user_message", "turn_done", "startup")
	app := eventApp(t, "", s.URL) // the plugin asks for everything, the operator allowed nothing
	app.Handle(context.Background(), msg(42, "你好"))
	app.emit("startup", 42, nil)
	app.Wait()
	if s.count() != 0 {
		t.Fatalf("nothing may be sent unless PLUGIN_EVENTS allows it: %v", s.events)
	}
}

func TestPluginMustAskForAnEvent(t *testing.T) {
	silent := newEventSink(t, 204)             // declares nothing: an old plugin
	picky := newEventSink(t, 204, "turn_done") // wants only turn_done
	greedy := newEventSink(t, 204, "user_message", "turn_done")
	app := eventApp(t, "all", silent.URL, picky.URL, greedy.URL)
	app.Handle(context.Background(), msg(42, "你好"))
	app.Wait()
	if silent.count() != 0 {
		t.Fatalf("a plugin that did not ask must hear nothing: %v", silent.events)
	}
	if got := picky.types(); !equalStrings(got, []string{"turn_done"}) {
		t.Fatalf("picky plugin: %v", got)
	}
	if got := greedy.types(); !equalStrings(got, []string{"user_message", "turn_done"}) {
		t.Fatalf("events must arrive in order: %v", got)
	}
}

func TestOperatorCanAllowOnlySomeEventTypes(t *testing.T) {
	s := newEventSink(t, 204, "user_message", "turn_done")
	app := eventApp(t, "user_message", s.URL)
	app.Handle(context.Background(), msg(42, "你好"))
	app.Wait()
	if got := s.types(); !equalStrings(got, []string{"user_message"}) {
		t.Fatalf("only the allowed type may be sent: %v", got)
	}
}

func TestEventEnvelope(t *testing.T) {
	s := newEventSink(t, 204, "user_message", "turn_done")
	app := eventApp(t, "all", s.URL)
	app.Handle(context.Background(), msg(42, "。"))
	app.Wait()
	um, td := s.first("user_message"), s.first("turn_done")
	if um == nil || td == nil {
		t.Fatalf("want user_message and turn_done, got %v", s.events)
	}
	for _, e := range []map[string]any{um, td} {
		if e["channel"] != "telegram" || e["chat"] != float64(42) || e["at"] == "" || e["tz"] == nil {
			t.Fatalf("bad envelope: %v", e)
		}
	}
	if d := um["data"].(map[string]any); d["kind"] != "text" {
		t.Fatalf("user_message data: %v", d)
	}
	if d := td["data"].(map[string]any); d["origin"] != "user" || d["replied"] != true {
		t.Fatalf("turn_done data: %v", d)
	}
	if s.auth[0] != "Bearer tok" {
		t.Fatalf("plugin token not sent: %q", s.auth[0])
	}
	if _, leaked := um["text"]; leaked {
		t.Fatal("message text must never be sent to plugins")
	}
}

func TestCommandsAreUserMessagesToo(t *testing.T) {
	s := newEventSink(t, 204, "user_message")
	app := eventApp(t, "all", s.URL)
	app.Handle(context.Background(), msg(42, "/help"))
	app.Wait()
	if e := s.first("user_message"); e == nil || e["data"].(map[string]any)["kind"] != "command" {
		t.Fatalf("a slash command is activity too: %v", s.events)
	}
}

func TestScheduledTurnsSendTurnDoneButNotUserMessage(t *testing.T) {
	s := newEventSink(t, 204, "user_message", "turn_done")
	app := eventApp(t, "all", s.URL)
	app.runScheduled(42, "晨报", "说早安")
	app.Wait()
	if got := s.types(); !equalStrings(got, []string{"turn_done"}) {
		t.Fatalf("a scheduled turn is not the user speaking: %v", got)
	}
	if d := s.first("turn_done")["data"].(map[string]any); d["origin"] != "scheduled" {
		t.Fatalf("turn_done data: %v", d)
	}
}

func TestNoEventsForStrangers(t *testing.T) {
	s := newEventSink(t, 204, "user_message", "turn_done")
	app := eventApp(t, "all", s.URL)
	app.Handle(context.Background(), msg(99, "你好")) // not on the allow list
	app.Wait()
	if s.count() != 0 {
		t.Fatalf("unexpected events: %v", s.events)
	}
}

func TestStartupEvent(t *testing.T) {
	s := newEventSink(t, 204, "startup")
	app := eventApp(t, "all", s.URL)
	app.emit("startup", app.defaultChat(), nil)
	app.Wait()
	if e := s.first("startup"); e == nil || e["chat"] != float64(42) {
		t.Fatalf("startup: %v", s.events)
	}
}

func TestEventToleratesNotFoundAndDeadPlugins(t *testing.T) {
	nf := newEventSink(t, 404, "user_message")
	dead := newEventSink(t, 204, "user_message")
	app := eventApp(t, "all", nf.URL, dead.URL)
	dead.Close()
	app.Handle(context.Background(), msg(42, "你好"))
	app.Wait()
	if nf.count() != 1 {
		t.Fatalf("a 404 plugin must still have been told once: %v", nf.events)
	}
}

func TestEventsDoNotWaitForTheChatLockOrASlowPlugin(t *testing.T) {
	fast := newEventSink(t, 204, "user_message")
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tools" {
			json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "events": []string{"user_message"}})
			return
		}
		<-release
	}))
	defer slow.Close()
	defer close(release)
	app := eventApp(t, "all", slow.URL, fast.URL)
	l := app.chatLock(42)
	l.Lock() // a long scheduled turn is running
	done := make(chan struct{})
	go func() { app.Handle(context.Background(), msg(42, "你好")); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for fast.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := fast.count()
	l.Unlock()
	<-done
	if got != 1 {
		t.Fatalf("the event must reach a healthy plugin while the turn is blocked and another plugin hangs, got %d", got)
	}
}

func TestEventQueueDropsOldestWhenAPluginIsStuck(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []float64
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tools" {
			json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "events": []string{"user_message"}})
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		seen = append(seen, b["chat"].(float64))
		mu.Unlock()
		<-release
	}))
	defer stuck.Close()
	app := eventApp(t, "all", stuck.URL)
	for i := 1; i <= pluginEventQueue+10; i++ {
		app.emit("user_message", int64(i), nil)
	}
	q := app.eventQueueFor(stuck.URL)
	q.mu.Lock()
	n := len(q.pending)
	q.mu.Unlock()
	if n > pluginEventQueue {
		t.Fatalf("queue grew to %d", n)
	}
	close(release)
	app.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || len(seen) > pluginEventQueue+1 { // the one in flight when it got stuck + a full queue
		t.Fatalf("a stuck plugin may hold at most %d events, it received %d", pluginEventQueue+1, len(seen))
	}
	if last := seen[len(seen)-1]; last != float64(pluginEventQueue+10) {
		t.Fatalf("the newest event must survive, last seen %v", last)
	}
}
