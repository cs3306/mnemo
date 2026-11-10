package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	pluginEventTimeout = 3 * time.Second
	pluginEventQueue   = 64
)

// Plugins can also ask to be told what happens in the conversation (to reset an idle timer, keep a presence status,
// log activity, ...). Nothing is sent unless BOTH sides agree:
//
//  1. the operator allows the event type in PLUGIN_EVENTS (default: none; "true" = every type), and
//  2. the plugin server lists the type in its GET /v1/tools answer: {"tools":[...],"events":["user_message","turn_done"]}.
//
// Each event is one POST {base}/v1/events with the same bearer token as tool calls:
//
//	{"type":"turn_done","chat":123,"channel":"telegram","at":"<RFC3339>","tz":"America/Vancouver","data":{...}}
//
// Types and their "data":
//
//	user_message  a real message from an allowed user arrived (sent before the turn, never waits for the model)
//	              data: {"kind":"text"|"voice"|"image"|"file"|"command"}
//	turn_done     mnemo finished a turn and sent whatever it had to say
//	              data: {"origin":"user"|"scheduled","replied":true|false}
//	startup       mnemo (re)started and discovered the plugins; chat is the default chat
//
// Message text is never included. Any 2xx answer is fine; 404 (the plugin does not care) is ignored. Events to one
// plugin are delivered in order, one at a time; a slow or dead plugin only delays its own events (at most
// pluginEventQueue are kept, the oldest are dropped) and can never delay a reply.
type eventQueue struct {
	mu      sync.Mutex
	pending [][]byte
	active  bool
}

func (a *App) eventQueueFor(base string) *eventQueue {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	if a.evQ == nil {
		a.evQ = map[string]*eventQueue{}
	}
	q := a.evQ[base]
	if q == nil {
		q = &eventQueue{}
		a.evQ[base] = q
	}
	return q
}

// emit announces an event to every plugin that wants it. It returns at once.
func (a *App) emit(typ string, chat int64, data map[string]any) {
	if !a.cfg.PluginEvents[typ] || len(a.cfg.PluginURLs) == 0 {
		return
	}
	var targets []string
	a.plug.mu.Lock()
	for _, base := range a.cfg.PluginURLs {
		if st := a.plug.urls[base]; st != nil && st.events[typ] {
			targets = append(targets, base)
		}
	}
	a.plug.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{
		"type": typ, "chat": chat, "channel": "telegram",
		"at": a.now().Format(time.RFC3339), "tz": a.cfg.TZ, "data": data,
	})
	for _, base := range targets {
		a.enqueueEvent(base, body)
	}
}

func (a *App) enqueueEvent(base string, body []byte) {
	q := a.eventQueueFor(base)
	q.mu.Lock()
	if len(q.pending) >= pluginEventQueue {
		q.pending = q.pending[1:]
		slog.Warn("plugin event queue full, dropped the oldest event", "url", base)
	}
	q.pending = append(q.pending, body)
	start := !q.active
	q.active = true
	q.mu.Unlock()
	if !start {
		return
	}
	a.goBG(func() {
		for {
			q.mu.Lock()
			if len(q.pending) == 0 {
				q.active = false
				q.mu.Unlock()
				return
			}
			next := q.pending[0]
			q.pending = q.pending[1:]
			q.mu.Unlock()
			a.postEvent(base, next)
		}
	})
}

func (a *App) postEvent(base string, body []byte) {
	ctx, cancel := context.WithTimeout(a.bg, pluginEventTimeout)
	defer cancel()
	req, err := a.pluginReq(ctx, http.MethodPost, base+"/v1/events", body)
	if err != nil {
		return
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		slog.Warn("plugin event failed", "url", base, "err", cleanNetErr(err))
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode/100 == 2:
		slog.Debug("plugin event sent", "url", base)
	case resp.StatusCode != http.StatusNotFound:
		slog.Warn("plugin event rejected", "url", base, "status", resp.StatusCode)
	}
}
