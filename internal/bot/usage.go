package bot

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"companion/internal/llm"
	"companion/internal/store"
)

// usageTotals are cumulative token counts for one kind of call ("chat", "maint", "vision").
type usageTotals struct{ Calls, Hit, Miss, Out, Reasoning int }

type tracker struct {
	mu sync.Mutex
	st *store.Store
}

func (t *tracker) get(kind string) usageTotals {
	var u usageTotals
	f := strings.Split(t.st.Meta("usage."+kind), ",")
	if len(f) != 5 {
		return u
	}
	n := make([]int, 5)
	for i := range n {
		n[i], _ = strconv.Atoi(f[i])
	}
	return usageTotals{n[0], n[1], n[2], n[3], n[4]}
}

func (t *tracker) hook(kind string) func(llm.Usage) {
	return func(d llm.Usage) {
		t.mu.Lock()
		defer t.mu.Unlock()
		u := t.get(kind)
		u.Calls++
		u.Hit += d.Hit
		u.Miss += d.Miss
		u.Out += d.Out
		u.Reasoning += d.Reasoning
		_ = t.st.SetMeta("usage."+kind, fmt.Sprintf("%d,%d,%d,%d,%d", u.Calls, u.Hit, u.Miss, u.Out, u.Reasoning))
	}
}

// describe renders totals for /status. The cost line appears only when PRICE_* are configured.
func (a *App) describeUsage(kind, label string) string {
	u := a.tr.get(kind)
	if u.Calls == 0 {
		return ""
	}
	in := u.Hit + u.Miss
	rate := 0.0
	if in > 0 {
		rate = 100 * float64(u.Hit) / float64(in)
	}
	s := fmt.Sprintf("%s：%d 次，输入 %d（缓存命中 %.0f%%），输出 %d", label, u.Calls, in, rate, u.Out)
	if u.Reasoning > 0 {
		s += fmt.Sprintf("（含思考 %d）", u.Reasoning)
	}
	c := a.cfg
	if c.PriceHit > 0 || c.PriceMiss > 0 || c.PriceOut > 0 {
		cost := (float64(u.Hit)*c.PriceHit + float64(u.Miss)*c.PriceMiss + float64(u.Out)*c.PriceOut) / 1e6
		s += fmt.Sprintf("，约 %.2f %s", cost, c.PriceCurrency)
	}
	return s
}
