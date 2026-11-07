package bot

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"companion/internal/llm"
)

const rawSuffix = "-raw"

// Handler returns the OpenAI-compatible HTTP API:
//
//	POST /v1/chat/completions   model "<name>" = with memory/persona (only the last user message is used),
//	                            model "<name>-raw" = plain pass-through to the chat backend (no memory)
//	GET  /v1/models
//	GET  /v1/memory?q=…         list/search memories      DELETE /v1/memory/{id}
//	GET  /healthz               (no auth)
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /v1/models", a.auth(a.apiModels))
	mux.HandleFunc("POST /v1/chat/completions", a.auth(a.apiChat))
	mux.HandleFunc("GET /v1/memory", a.auth(a.apiMemoryList))
	mux.HandleFunc("DELETE /v1/memory/{id}", a.auth(a.apiMemoryDelete))
	return mux
}

func (a *App) serveAPI(ctx context.Context) error {
	srv := &http.Server{Addr: a.cfg.APIAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()
	slog.Info("API listening", "addr", a.cfg.APIAddr, "model", a.cfg.APIModel)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if a.cfg.APIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.APIKey)) != 1 {
			apiError(w, 401, "invalid API key")
			return
		}
		next(w, r)
	}
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "companion_error"}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *App) apiModels(w http.ResponseWriter, r *http.Request) {
	m := func(id string) map[string]any {
		return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "companion"}
	}
	writeJSON(w, map[string]any{"object": "list", "data": []any{m(a.cfg.APIModel), m(a.cfg.APIModel + rawSuffix)}})
}

type chatReq struct {
	Model       string            `json:"model"`
	Messages    []json.RawMessage `json:"messages"`
	Stream      bool              `json:"stream"`
	Temperature *float64          `json:"temperature"`
	User        string            `json:"user"`
	Tools       json.RawMessage   `json:"tools"`
}

type rawMsg struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
}

// defaultChat is the conversation shared with Telegram so memory is common to all frontends.
func (a *App) defaultChat() int64 {
	var ids []int64
	for id := range a.cfg.AllowedIDs {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 1
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids[0]
}

// chatFor maps a request to a conversation: header X-Companion-Chat or the "user" field; empty = shared default.
func (a *App) chatFor(r *http.Request, user string) int64 {
	key := strings.TrimSpace(r.Header.Get("X-Companion-Chat"))
	if key == "" {
		key = strings.TrimSpace(user)
	}
	if key == "" {
		return a.defaultChat()
	}
	h := fnv.New64a()
	h.Write([]byte(key))
	return -int64(h.Sum64()>>2) - 1 // negative: can never collide with a Telegram chat ID
}

// parseContent returns the text and data-URL images of an OpenAI message content (string or parts array).
func parseContent(raw json.RawMessage) (string, []Image, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("unsupported message content")
	}
	var texts []string
	var imgs []Image
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image_url":
			u := p.ImageURL.URL
			rest, ok := strings.CutPrefix(u, "data:")
			meta, b64, ok2 := strings.Cut(rest, ",")
			if !ok || !ok2 || !strings.HasSuffix(meta, ";base64") {
				return "", nil, errors.New("only base64 data: image URLs are supported (the server does not fetch remote URLs)")
			}
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return "", nil, errors.New("invalid base64 image")
			}
			imgs = append(imgs, Image{Data: data, Mime: strings.TrimSuffix(meta, ";base64")})
		}
	}
	return strings.Join(texts, "\n"), imgs, nil
}

func (a *App) apiChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, 400, "invalid JSON body")
		return
	}
	if len(req.Messages) == 0 {
		apiError(w, 400, "messages is empty")
		return
	}
	temp := a.cfg.Temperature
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	var reply Reply
	var err error
	if req.Model == a.cfg.APIModel+rawSuffix {
		reply, err = a.rawChat(r.Context(), req, temp)
	} else {
		reply, err = a.memoryChat(r.Context(), r, req)
	}
	if err != nil {
		code := 502
		var he *httpErr
		if errors.As(err, &he) {
			code = he.code
		}
		slog.Warn("API chat failed", "err", err)
		apiError(w, code, truncate(err.Error(), 300))
		return
	}
	a.writeCompletion(w, req, reply)
}

type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

// toolText turns a tool-result content (string or parts) into text; images are transcribed by the vision model.
func (a *App) toolText(ctx context.Context, raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.HasPrefix(s, "data:image/") {
			raw, _ = json.Marshal([]map[string]any{{"type": "image_url", "image_url": map[string]string{"url": s}}})
		} else {
			return s, nil
		}
	}
	text, imgs, err := parseContent(raw)
	if err != nil {
		return "", &httpErr{400, err.Error()}
	}
	if len(imgs) > 0 {
		desc, err := a.describeImages(ctx, imgs)
		if errors.Is(err, errVisionOff) {
			return "", &httpErr{400, err.Error()}
		}
		if err != nil {
			return "", err
		}
		text = strings.TrimSpace(text + "\n" + desc)
	}
	return text, nil
}

func (a *App) convertTail(ctx context.Context, raws []json.RawMessage) ([]llm.Message, error) {
	var out []llm.Message
	for _, rm := range raws {
		var m rawMsg
		if err := json.Unmarshal(rm, &m); err != nil {
			return nil, &httpErr{400, "invalid message"}
		}
		switch m.Role {
		case "assistant":
			text, _, _ := parseContent(m.Content) // null/absent content -> ""
			out = append(out, llm.Message{Role: "assistant", Content: text, ToolCalls: m.ToolCalls, ReasoningContent: m.ReasoningContent})
		case "tool":
			text, err := a.toolText(ctx, m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, llm.Message{Role: "tool", ToolCallID: m.ToolCallID, Content: text})
		}
	}
	return out, nil
}

func (a *App) memoryChat(ctx context.Context, r *http.Request, req chatReq) (Reply, error) {
	last := -1
	var lm rawMsg
	for i := len(req.Messages) - 1; i >= 0; i-- {
		var m rawMsg
		if json.Unmarshal(req.Messages[i], &m) == nil && m.Role == "user" {
			last, lm = i, m
			break
		}
	}
	if last < 0 {
		return Reply{}, &httpErr{400, "no user message"}
	}
	text, imgs, err := parseContent(lm.Content)
	if err != nil {
		return Reply{}, &httpErr{400, err.Error()}
	}
	tail, err := a.convertTail(ctx, req.Messages[last+1:])
	if err != nil {
		return Reply{}, err
	}
	chat := a.chatFor(r, req.User)
	l := a.chatLock(chat)
	l.Lock()
	defer l.Unlock()
	a.touch(chat)
	defer a.touch(chat)
	reply, err := a.RespondTools(ctx, chat, strings.TrimSpace(text), imgs, TurnOpts{Tools: req.Tools, Tail: tail})
	if errors.Is(err, errVisionOff) {
		return Reply{}, &httpErr{400, err.Error()}
	}
	return reply, err
}

// rawChat forwards the client's own messages to the chat backend: no memory, no persona, nothing stored.
// Useful for frontends' auxiliary requests (titles, tags) so they do not pollute memory.
func (a *App) rawChat(ctx context.Context, req chatReq, temp float64) (Reply, error) {
	var msgs []llm.Message
	for _, rm := range req.Messages {
		var m llm.Message
		if err := json.Unmarshal(rm, &m); err != nil {
			return Reply{}, &httpErr{400, "invalid message"}
		}
		msgs = append(msgs, m)
	}
	if len(bytes.TrimSpace(req.Tools)) > 0 && string(bytes.TrimSpace(req.Tools)) != "null" {
		tc, ok := a.chat.(llm.ToolCompleter)
		if !ok {
			return Reply{}, &httpErr{400, "the chat backend does not support tools"}
		}
		res, err := tc.CompleteTools(ctx, msgs, req.Tools, temp)
		return Reply{Text: res.Text, ToolCalls: res.ToolCalls, Reasoning: res.Reasoning}, err
	}
	text, err := a.chat.Complete(ctx, msgs, temp)
	return Reply{Text: text}, err
}

// writeCompletion sends the finished reply (or tool-call request) as a JSON completion or as SSE chunks. The
// backend call is not streamed upstream: the full reply is produced first (so memory bookkeeping is consistent
// and tool calls are complete), then replayed.
func (a *App) writeCompletion(w http.ResponseWriter, req chatReq, rep Reply) {
	if req.Model == "" {
		req.Model = a.cfg.APIModel
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	finish := "stop"
	var calls []map[string]any
	if len(rep.ToolCalls) > 0 {
		finish = "tool_calls"
		_ = json.Unmarshal(rep.ToolCalls, &calls)
	}
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": rep.Text}
		if finish == "tool_calls" {
			msg["tool_calls"] = rep.ToolCalls
			if rep.Text == "" {
				msg["content"] = nil
			}
		}
		if rep.Reasoning != "" {
			msg["reasoning_content"] = rep.Reasoning
		}
		writeJSON(w, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": msg}},
			"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, fin any) {
		b, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	if rep.Reasoning != "" {
		chunk(map[string]any{"reasoning_content": rep.Reasoning}, nil)
	}
	rs := []rune(rep.Text)
	for i := 0; i < len(rs); i += 6 {
		chunk(map[string]any{"content": string(rs[i:min(i+6, len(rs))])}, nil)
	}
	if finish == "tool_calls" {
		for i := range calls {
			calls[i]["index"] = i
		}
		chunk(map[string]any{"tool_calls": calls}, nil)
	}
	chunk(map[string]any{}, finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func (a *App) apiMemoryList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	mems := a.st.ActiveMemories()
	if q != "" {
		var err error
		if mems, err = a.mm.Retrieve(r.Context(), q, 30); err != nil {
			apiError(w, 502, truncate(err.Error(), 300))
			return
		}
	} else {
		sort.Slice(mems, func(i, j int) bool { return mems[i].ID > mems[j].ID })
		if len(mems) > 200 {
			mems = mems[:200]
		}
	}
	out := make([]map[string]any, 0, len(mems))
	for _, m := range mems {
		out = append(out, map[string]any{"id": m.ID, "kind": m.Kind, "text": m.Text, "importance": m.Importance, "by_model": m.ByModel})
	}
	writeJSON(w, map[string]any{"data": out, "core": a.st.Core()})
}

func (a *App) apiMemoryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, 400, "bad id")
		return
	}
	ok, err := a.st.DeleteMemory(id)
	switch {
	case err != nil:
		apiError(w, 500, truncate(err.Error(), 300))
	case !ok:
		apiError(w, 404, "no such memory")
	default:
		writeJSON(w, map[string]any{"deleted": id})
	}
}
