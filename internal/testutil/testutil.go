// Package testutil holds fakes shared by tests.
package testutil

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math"
	"sync"

	"companion/internal/llm"
	"companion/internal/textutil"
)

// Embed is a deterministic bag-of-bigrams embedder; different Name => different vector space.
type Embed struct {
	Name string
	Dim  int
}

func (e *Embed) Model() string { return e.Name }

func (e *Embed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dim := e.Dim
	if dim == 0 {
		dim = 64
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dim)
		for g := range textutil.Bigrams(t) {
			h := fnv.New32a()
			h.Write([]byte(e.Name + g))
			v[h.Sum32()%uint32(dim)]++
		}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		if n == 0 {
			v[0], n = 1, 1
		}
		for j := range v {
			v[j] /= float32(math.Sqrt(n))
		}
		out[i] = v
	}
	return out, nil
}

// Chat returns scripted replies in order (last one repeats) and records the prompts.
type Chat struct {
	mu      sync.Mutex
	Replies []string
	Calls   [][]llm.Message
}

func (c *Chat) Complete(_ context.Context, msgs []llm.Message, _ float64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, msgs)
	if len(c.Replies) == 0 {
		return "ok", nil
	}
	i := len(c.Calls) - 1
	if i >= len(c.Replies) {
		i = len(c.Replies) - 1
	}
	return c.Replies[i], nil
}

// ToolChat is a scripted backend that supports tool calls. The last script entry repeats.
type ToolChat struct {
	mu        sync.Mutex
	Script    []llm.Result
	Calls     [][]llm.Message
	ToolsSeen []json.RawMessage
}

func (c *ToolChat) CompleteTools(_ context.Context, msgs []llm.Message, tools json.RawMessage, _ float64) (llm.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, msgs)
	c.ToolsSeen = append(c.ToolsSeen, tools)
	i := len(c.Calls) - 1
	if i >= len(c.Script) {
		i = len(c.Script) - 1
	}
	return c.Script[i], nil
}

func (c *ToolChat) Complete(ctx context.Context, msgs []llm.Message, t float64) (string, error) {
	r, err := c.CompleteTools(ctx, msgs, nil, t)
	return r.Text, err
}
