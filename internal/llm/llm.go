// Package llm is a small client for OpenAI-compatible chat and embedding endpoints.
// llama-server, vLLM, DeepSeek and others differ only in base URL, key, model and extra fields.
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"companion/internal/config"
	"companion/internal/textutil"
)

// RetryDelay is the base back-off between retries (tests shorten it).
var RetryDelay = 1500 * time.Millisecond

type Message struct {
	Role             string          `json:"role"`
	Content          any             `json:"content"` // string, or []Part for multimodal
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// Usage is the token accounting of one completion. Hit/Miss follow DeepSeek's context-cache fields;
// for servers without them everything counts as Miss.
type Usage struct {
	Hit, Miss, Out, Reasoning int
}

// Result is a completion that may ask for tool calls instead of (or besides) text.
type Result struct {
	Text      string
	ToolCalls json.RawMessage // OpenAI "tool_calls" array, nil if none
	Reasoning string          // reasoning_content, when the backend thinks; must be echoed back with tool calls
}

// ToolCompleter is implemented by clients that can pass OpenAI "tools" through.
type ToolCompleter interface {
	CompleteTools(ctx context.Context, msgs []Message, tools json.RawMessage, temperature float64) (Result, error)
}

type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

func Text(role, s string) Message { return Message{Role: role, Content: s} }

// Image builds a user message with a text prompt and an inline image.
func Image(prompt string, data []byte, mime string) Message {
	url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	return Message{Role: "user", Content: []Part{
		{Type: "text", Text: prompt},
		{Type: "image_url", ImageURL: &ImageURL{URL: url}},
	}}
}

type Completer interface {
	Complete(ctx context.Context, msgs []Message, temperature float64) (string, error)
}

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

type Client struct {
	EP      config.Endpoint
	HC      *http.Client
	OnUsage func(Usage) // optional; called after every successful chat completion

	// midSystemAsUser is set once the server rejected a system message that is not first (strict chat templates such
	// as Qwen3's raise "System message must be at the beginning"); from then on later system messages are sent as user.
	midSystemAsUser atomic.Bool
}

// systemNotePrefix marks a mid-conversation system message that had to be sent as a user message.
const systemNotePrefix = "[系统提示] "

// demoteMidSystem returns msgs with every system message after the first position turned into a user message.
// The input slice is not modified. Non-string contents (multimodal parts) are left alone.
func demoteMidSystem(msgs []Message) ([]Message, bool) {
	changed := false
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := 1; i < len(out); i++ {
		if out[i].Role != "system" {
			continue
		}
		if s, ok := out[i].Content.(string); ok {
			out[i].Role, out[i].Content = "user", systemNotePrefix+s
			changed = true
		}
	}
	return out, changed
}

// isSystemPositionError recognises the chat-template error of backends that only allow a leading system message.
func isSystemPositionError(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) || he.Status < 400 {
		return false
	}
	b := strings.ToLower(he.Body)
	return strings.Contains(b, "system message must be at the beginning") ||
		strings.Contains(b, "system message must be first")
}

func New(ep config.Endpoint) *Client { return &Client{EP: ep, HC: &http.Client{}} }

func (c *Client) Model() string { return c.EP.Model }

// HTTPError is a non-2xx response from the service.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// retriable: rate limits and server errors, except a chat-template rejection, which no retry can fix.
func (e *HTTPError) retriable() bool {
	return (e.Status == 429 || e.Status >= 500) && !isSystemPositionError(e)
}

func (c *Client) Complete(ctx context.Context, msgs []Message, temperature float64) (string, error) {
	r, err := c.chat(ctx, msgs, nil, temperature)
	return r.Text, err
}

func (c *Client) CompleteTools(ctx context.Context, msgs []Message, tools json.RawMessage, temperature float64) (Result, error) {
	return c.chat(ctx, msgs, tools, temperature)
}

func (c *Client) chat(ctx context.Context, msgs []Message, tools json.RawMessage, temperature float64) (Result, error) {
	if c.midSystemAsUser.Load() {
		msgs, _ = demoteMidSystem(msgs)
	}
	payload := map[string]any{
		"model": c.EP.Model, "messages": msgs, "temperature": temperature, "stream": false,
	}
	for k, v := range c.EP.ExtraBody {
		payload[k] = v
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int  `json:"prompt_tokens"`
			CompletionTokens int  `json:"completion_tokens"`
			CacheHit         *int `json:"prompt_cache_hit_tokens"`
			CacheMiss        *int `json:"prompt_cache_miss_tokens"`
			PromptDetails    struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				Reasoning int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	err := c.post(ctx, "/chat/completions", payload, &resp)
	if err != nil && isSystemPositionError(err) && !c.midSystemAsUser.Load() {
		if fixed, changed := demoteMidSystem(msgs); changed {
			c.midSystemAsUser.Store(true)
			payload["messages"] = fixed
			err = c.post(ctx, "/chat/completions", payload, &resp)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if len(resp.Choices) == 0 {
		return Result{}, errors.New("chat endpoint returned no choices")
	}
	if c.OnUsage != nil {
		u := resp.Usage
		hit := u.PromptDetails.Cached
		if u.CacheHit != nil {
			hit = *u.CacheHit
		}
		miss := max(u.PromptTokens-hit, 0)
		if u.CacheMiss != nil {
			miss = *u.CacheMiss
		}
		c.OnUsage(Usage{Hit: hit, Miss: miss, Out: u.CompletionTokens, Reasoning: u.CompletionDetails.Reasoning})
	}
	m := resp.Choices[0].Message
	r := Result{Text: textutil.StripThink(m.Content), Reasoning: m.ReasoningContent}
	if t := bytes.TrimSpace(m.ToolCalls); len(t) > 2 && string(t) != "null" { // "[]" and null mean none
		r.ToolCalls = m.ToolCalls
	}
	return r, nil
}

// Balance returns the account balance text from DeepSeek's GET /user/balance (not available on other servers).
func (c *Client) Balance(ctx context.Context) (string, error) {
	hc := c.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.EP.BaseURL+"/user/balance", nil)
	if err != nil {
		return "", err
	}
	if c.EP.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.EP.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", &netError{err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("balance not available (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Available bool `json:"is_available"`
		Infos     []struct {
			Currency string `json:"currency"`
			Total    string `json:"total_balance"`
			Granted  string `json:"granted_balance"`
			ToppedUp string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Infos) == 0 {
		return "", errors.New("unexpected balance response")
	}
	var sb []string
	for _, i := range out.Infos {
		sb = append(sb, fmt.Sprintf("%s %s（赠送 %s，充值 %s）", i.Currency, i.Total, i.Granted, i.ToppedUp))
	}
	s := ""
	for i, x := range sb {
		if i > 0 {
			s += "；"
		}
		s += x
	}
	if !out.Available {
		s += "（账户当前不可用）"
	}
	return s, nil
}

// Embed returns L2-normalised vectors, one per input, in input order.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	const batch = 32
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batch {
		end := min(i+batch, len(texts))
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		payload := map[string]any{"model": c.EP.Model, "input": texts[i:end]}
		if err := c.post(ctx, "/embeddings", payload, &resp); err != nil {
			return nil, err
		}
		if len(resp.Data) != end-i {
			return nil, fmt.Errorf("embeddings endpoint returned %d vectors for %d inputs", len(resp.Data), end-i)
		}
		sort.Slice(resp.Data, func(a, b int) bool { return resp.Data[a].Index < resp.Data[b].Index })
		for _, d := range resp.Data {
			out = append(out, normalize(d.Embedding))
		}
	}
	return out, nil
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	if n == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

func (c *Client) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hc := c.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	timeout := c.EP.Timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	var last error
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(RetryDelay * time.Duration(attempt)):
			}
		}
		last = func() error {
			rctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.EP.BaseURL+path, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if c.EP.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+c.EP.APIKey)
			}
			resp, err := hc.Do(req)
			if err != nil {
				return &netError{err}
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			if err != nil {
				return &netError{err}
			}
			if resp.StatusCode >= 400 {
				msg := string(b)
				if len(msg) > 300 {
					msg = msg[:300]
				}
				return &HTTPError{Status: resp.StatusCode, Body: msg}
			}
			return json.Unmarshal(b, out)
		}()
		if last == nil {
			return nil
		}
		var he *HTTPError
		var ne *netError
		retry := (errors.As(last, &he) && he.retriable()) || errors.As(last, &ne)
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("%s%s: %w", c.EP.BaseURL, path, last)
}

type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }
