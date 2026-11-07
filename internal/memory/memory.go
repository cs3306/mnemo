// Package memory implements long-term memory: retrieval (turn start), extraction and rolling summary
// (turn end / idle), and consolidation (nightly). Memories are plain text, so they outlive any model swap.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/store"
	"companion/internal/textutil"
)

const (
	dupSim   = 0.93 // near-identical: reinforce the existing memory
	checkSim = 0.80 // related: ask the model whether to add / update / ignore
	mergeSim = 0.95 // consolidation: treat as duplicates
)

const extractPrompt = `你是记忆整理器。下面是用户与助手的一段对话，今天是 {{today}}。
请只提取【用户明确说过或确认过】的、值得长期记住的信息。

规则：
- 助手的建议、解释、推测一律不要提取；用户没有确认的内容不要提取。
- 每条一句话，自包含（写清主语）；相对时间换成绝对日期（如“下周三”→具体日期）。
- 一次性的寒暄、临时情绪、没有长期价值的内容不要提取。
- 不要提取证件号、银行卡号、密码等敏感标识符。
- importance：1=琐碎，3=一般，5=很重要（长期目标、重要关系、关键约束）。
- kind：fact（事实）/ preference（偏好）/ event（事件）/ plan（计划）。
- 如果涉及学习，另外在 learning 里记录：topic（主题）、status（weak/learning/solid）、error_pattern（用户反复犯的具体错误，没有则留空）。

只输出 JSON，不要其他文字：
{"memories":[{"kind":"fact","text":"…","importance":3}],"learning":[{"topic":"…","status":"weak","error_pattern":""}]}
没有可提取的内容时输出 {"memories":[],"learning":[]}。

【对话】
{{dialogue}}`

const decidePrompt = `已有记忆：{{old}}
新信息：{{new}}
判断如何处理，只输出 JSON：
- 新信息与已有记忆重复或没有新内容 → {"action":"noop"}
- 新信息更新/修正了已有记忆 → {"action":"update","text":"合并后的一句话"}
- 两者是不同的事实 → {"action":"add"}`

const summaryPrompt = `把下面的对话压缩进滚动摘要。保留：用户做了什么、讨论了什么结论、未完成的事、重要的情绪或语境转折。
不要逐句复述，不要加评价。输出不超过 600 字的中文摘要，只输出摘要本身。

【已有摘要】
{{summary}}

【需要并入的新对话】
{{dialogue}}`

const corePrompt = `你在维护一份关于用户的“核心档案”，它会常驻在每次对话的开头，所以必须精炼。
请根据旧档案和最新的重要记忆，重写档案：
- 不超过 {{limit}} 字，中文，分条或短段落均可。
- 只保留稳定、长期有效、对日常对话有帮助的信息（身份、目标、长期偏好、重要关系、关键约束、当前主要项目）。
- 旧档案里仍然成立的内容要保留，被新记忆明确推翻的才修改。不要凭空添加记忆里没有的内容。
- 只输出档案正文。

【旧档案】
{{core}}

【重要记忆】
{{memories}}`

func fill(tmpl string, kv ...string) string {
	return strings.NewReplacer(kv...).Replace(tmpl)
}

// flexInt accepts 3, 3.0 or "3" (models are not consistent).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 3
		return nil
	}
	*f = flexInt(int(math.Round(v)))
	return nil
}

type extractOut struct {
	Memories []struct {
		Kind       string  `json:"kind"`
		Text       string  `json:"text"`
		Importance flexInt `json:"importance"`
	} `json:"memories"`
	Learning []struct {
		Topic        string `json:"topic"`
		Status       string `json:"status"`
		ErrorPattern string `json:"error_pattern"`
	} `json:"learning"`
}

type Manager struct {
	St         *store.Store
	Emb        llm.Embedder
	Maint      llm.Completer
	MaintModel string
	Cfg        *config.Config
	Now        func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// usable reports whether a memory's vector is comparable with the current embedding model.
func (m *Manager) usable(mem store.Memory) bool {
	return len(mem.Emb) > 0 && mem.EmbedModel == m.Emb.Model()
}

// ---------------------------------------------------------------- retrieval

// Retrieve returns up to k memories ranked by relevance (cosine + keyword overlap), recency and
// importance. Memories whose vectors came from another embedding model fall back to keyword-only
// matching until Reembed has run.
func (m *Manager) Retrieve(ctx context.Context, query string, k int) ([]store.Memory, error) {
	if k <= 0 {
		k = m.Cfg.TopK
	}
	mems := m.St.ActiveMemories()
	if len(mems) == 0 {
		return nil, nil
	}
	qv, err := m.Emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	q := qv[0]
	qb := textutil.Bigrams(query)
	now := float64(m.now().UnixNano()) / 1e9

	type scored struct {
		s float64
		m store.Memory
	}
	var cands []scored
	for _, mem := range mems {
		kw := textutil.Overlap(qb, textutil.Bigrams(mem.Text))
		cos := 0.0
		if m.usable(mem) {
			cos = dot(q, mem.Emb)
			if cos < m.Cfg.MinSim && kw < 0.3 {
				continue
			}
		} else if kw < 0.3 {
			continue
		}
		rel := cos + 0.15*kw
		ref := mem.LastUsed
		if ref == 0 {
			ref = mem.Created
		}
		rec := math.Exp(-math.Max(0, now-ref) / 86400 / 60)
		imp := float64(mem.Importance) / 5
		cands = append(cands, scored{0.65*rel + 0.15*rec + 0.20*imp, mem})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].s > cands[j].s })
	var out []store.Memory
	used := 0
	for _, c := range cands {
		if len(out) >= k {
			break
		}
		t := textutil.EstTokens(c.m.Text)
		if len(out) > 0 && used+t > m.Cfg.MemoryTokens {
			break
		}
		out = append(out, c.m)
		used += t
	}
	return out, nil
}

func (m *Manager) Touch(mems []store.Memory) {
	ids := make([]int64, len(mems))
	for i, mem := range mems {
		ids[i] = mem.ID
	}
	if err := m.St.TouchMemories(ids); err != nil {
		slog.Warn("touch memories failed", "err", err)
	}
}

// ---------------------------------------------------------------- extraction

func dialogue(rows []store.Message) string {
	var b strings.Builder
	for _, r := range rows {
		name := "用户"
		if r.Role == "assistant" {
			name = "助手"
		}
		b.WriteString(name + "：" + r.Content + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Extract processes messages since the last extraction. It returns the number of memories added/updated.
// If the model's reply cannot be parsed the cursor is NOT advanced, so the window is retried later.
func (m *Manager) Extract(ctx context.Context, chat int64) (int, error) {
	after := m.St.ExtractedUpTo(chat)
	rows := m.St.MessagesAfter(chat, after, 40)
	if len(rows) == 0 {
		return 0, nil
	}
	prompt := fill(extractPrompt, "{{today}}", m.now().Format("2006-01-02 Monday"), "{{dialogue}}", dialogue(rows))
	var out extractOut
	ok := false
	for attempt := 0; attempt < 2 && !ok; attempt++ {
		p := prompt
		if attempt == 1 {
			p += "\n\n（上一次的输出不是合法 JSON。请只输出 JSON 对象本身。）"
		}
		reply, err := m.Maint.Complete(ctx, []llm.Message{llm.Text("user", p)}, 0.1)
		if err != nil {
			return 0, err
		}
		out = extractOut{}
		ok = textutil.ExtractJSON(reply, &out)
	}
	if !ok {
		slog.Warn("memory extraction returned no parseable JSON; will retry later")
		return 0, nil
	}
	lastID := rows[len(rows)-1].ID
	var texts []string
	for _, c := range out.Memories {
		if t := strings.TrimSpace(c.Text); t != "" && len(texts) < 12 {
			texts = append(texts, t)
		}
	}
	changed := 0
	if len(texts) > 0 {
		vecs, err := m.Emb.Embed(ctx, texts)
		if err != nil {
			return 0, err
		}
		i := 0
		for _, c := range out.Memories {
			t := strings.TrimSpace(c.Text)
			if t == "" || i >= len(texts) {
				continue
			}
			n, err := m.integrate(ctx, c.Kind, t, vecs[i], int(c.Importance), lastID)
			if err != nil {
				return changed, err
			}
			changed += n
			i++
		}
	}
	for _, l := range out.Learning {
		topic := strings.TrimSpace(l.Topic)
		if topic == "" {
			continue
		}
		status := l.Status
		if status != "weak" && status != "learning" && status != "solid" {
			status = "learning"
		}
		if err := m.St.UpsertLearning(truncRunes(topic, 80), status, truncRunes(l.ErrorPattern, 200)); err != nil {
			return changed, err
		}
	}
	return changed, m.St.SetExtractedUpTo(chat, lastID)
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (m *Manager) integrate(ctx context.Context, kind, text string, emb []float32, importance int, srcMsg int64) (int, error) {
	if kind == "" {
		kind = "fact"
	}
	newMem := store.Memory{Kind: kind, Text: text, Importance: importance, Source: "user", ByModel: m.MaintModel,
		EmbedModel: m.Emb.Model(), SrcMsg: srcMsg, Emb: emb}

	var best store.Memory
	bestSim := -1.0
	for _, mem := range m.St.ActiveMemories() {
		if !m.usable(mem) {
			continue
		}
		if s := dot(emb, mem.Emb); s > bestSim {
			best, bestSim = mem, s
		}
	}
	if bestSim >= dupSim {
		return 0, m.St.BumpImportance(best.ID, importance)
	}
	if bestSim >= checkSim {
		reply, err := m.Maint.Complete(ctx, []llm.Message{llm.Text("user",
			fill(decidePrompt, "{{old}}", best.Text, "{{new}}", text))}, 0)
		if err != nil {
			return 0, err
		}
		var d struct {
			Action string `json:"action"`
			Text   string `json:"text"`
		}
		d.Action = "add"
		textutil.ExtractJSON(reply, &d)
		switch d.Action {
		case "noop":
			return 0, m.St.BumpImportance(best.ID, importance)
		case "update":
			if merged := strings.TrimSpace(d.Text); merged != "" {
				v, err := m.Emb.Embed(ctx, []string{merged})
				if err != nil {
					return 0, err
				}
				newMem.Text, newMem.Emb = merged, v[0]
				newMem.Importance = max(importance, best.Importance)
				id, err := m.St.AddMemory(newMem)
				if err != nil {
					return 0, err
				}
				return 1, m.St.Supersede(best.ID, id)
			}
		}
	}
	_, err := m.St.AddMemory(newMem)
	return 1, err
}

// ---------------------------------------------------------------- rolling summary

// RollSummary folds the oldest messages into the rolling summary once the verbatim history exceeds
// 125% of the budget, leaving ~60% of the budget. It never summarises messages that memory
// extraction has not seen yet.
func (m *Manager) RollSummary(ctx context.Context, chat int64) (bool, error) {
	rows := m.St.Unsummarized(chat)
	total := 0
	for _, r := range rows {
		total += textutil.EstTokens(r.Content)
	}
	if float64(total) <= float64(m.Cfg.RecentTokens)*1.25 {
		return false, nil
	}
	keepBudget := int(float64(m.Cfg.RecentTokens) * 0.6)
	keep, used := 0, 0
	for i := len(rows) - 1; i >= 0; i-- {
		t := textutil.EstTokens(rows[i].Content)
		if used+t > keepBudget {
			break
		}
		used += t
		keep++
	}
	old := rows[:len(rows)-keep]
	extracted := m.St.ExtractedUpTo(chat)
	n := 0
	for n < len(old) && old[n].ID <= extracted {
		n++
	}
	old = old[:n]
	if len(old) == 0 {
		return false, nil
	}
	prev := m.St.Summary(chat)
	if prev == "" {
		prev = "（无）"
	}
	reply, err := m.Maint.Complete(ctx, []llm.Message{llm.Text("user",
		fill(summaryPrompt, "{{summary}}", prev, "{{dialogue}}", dialogue(old)))}, 0.2)
	if err != nil {
		return false, err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return false, nil
	}
	return true, m.St.SetSummary(chat, truncRunes(reply, 1500), old[len(old)-1].ID)
}

// ---------------------------------------------------------------- consolidation

type Stats struct {
	Archived    int  `json:"archived"`
	Merged      int  `json:"merged"`
	CoreUpdated bool `json:"core_updated"`
}

// Consolidate backs up, archives stale low-value memories, merges near-duplicates, compacts the log and
// rewrites the core profile.
func (m *Manager) Consolidate(ctx context.Context) (Stats, error) {
	var st Stats
	if _, err := m.St.Backup(filepath.Join(m.Cfg.DataDir, "backups"), 7); err != nil {
		return st, fmt.Errorf("backup failed, aborting consolidation: %w", err)
	}
	now := float64(m.now().UnixNano()) / 1e9
	for _, mem := range m.St.ActiveMemories() {
		if mem.Importance <= 2 && mem.UseCount == 0 && now-mem.Created > 90*86400 {
			if err := m.St.SetStatus(mem.ID, "archived"); err != nil {
				return st, err
			}
			st.Archived++
		}
	}

	mems := m.St.ActiveMemories()
	if n := len(mems); n > 1 && n <= 3000 {
		gone := map[int64]bool{}
		for i := 0; i < n; i++ {
			if gone[mems[i].ID] {
				continue
			}
			for j := i + 1; j < n; j++ {
				a, b := mems[i], mems[j]
				if gone[b.ID] || !m.usable(a) || !m.usable(b) || dot(a.Emb, b.Emb) < mergeSim {
					continue
				}
				older, newer := a, b
				if a.Created > b.Created {
					older, newer = b, a
				}
				if err := m.St.Supersede(older.ID, newer.ID); err != nil {
					return st, err
				}
				if err := m.St.BumpImportance(newer.ID, older.Importance); err != nil {
					return st, err
				}
				gone[older.ID] = true
				st.Merged++
				if older.ID == a.ID {
					break
				}
			}
		}
	}
	if err := m.St.Compact(); err != nil {
		return st, err
	}

	top := m.St.ActiveMemories()
	sort.SliceStable(top, func(i, j int) bool {
		if top[i].Importance != top[j].Importance {
			return top[i].Importance > top[j].Importance
		}
		return top[i].Created > top[j].Created
	})
	if len(top) > 40 {
		top = top[:40]
	}
	if len(top) > 0 {
		var b strings.Builder
		for _, mem := range top {
			b.WriteString("- " + mem.Text + "\n")
		}
		core := m.St.Core()
		if core == "" {
			core = "（空）"
		}
		reply, err := m.Maint.Complete(ctx, []llm.Message{llm.Text("user",
			fill(corePrompt, "{{limit}}", strconv.Itoa(m.Cfg.CoreMaxChars), "{{core}}", core, "{{memories}}", b.String()))}, 0.2)
		if err != nil {
			return st, err
		}
		reply = strings.TrimSpace(reply)
		if reply != "" && len([]rune(reply)) <= m.Cfg.CoreMaxChars*2 && reply != m.St.Core() {
			if err := m.St.SetCore(reply, "consolidation"); err != nil {
				return st, err
			}
			st.CoreUpdated = true
		}
	}
	return st, m.St.SetMeta("last_consolidation", strconv.FormatInt(m.now().Unix(), 10))
}

// ---------------------------------------------------------------- model swap support

// NeedReembed counts memories whose vectors were produced by a different embedding model.
func (m *Manager) NeedReembed() int {
	n := 0
	for _, mem := range m.St.AllMemories() {
		if mem.Status != "superseded" && !m.usable(mem) {
			n++
		}
	}
	return n
}

// Reembed recomputes vectors for every memory that was embedded by another model (or never embedded,
// e.g. after Import). Text is untouched. Returns how many memories were updated.
func (m *Manager) Reembed(ctx context.Context) (int, error) {
	var todo []store.Memory
	for _, mem := range m.St.AllMemories() {
		if !m.usable(mem) {
			todo = append(todo, mem)
		}
	}
	const batch = 32
	done := 0
	for i := 0; i < len(todo); i += batch {
		chunk := todo[i:min(i+batch, len(todo))]
		texts := make([]string, len(chunk))
		for j, mem := range chunk {
			texts[j] = mem.Text
		}
		vecs, err := m.Emb.Embed(ctx, texts)
		if err != nil {
			return done, err
		}
		for j, mem := range chunk {
			if err := m.St.EditMemory(mem.ID, mem.Text, m.Emb.Model(), vecs[j]); err != nil {
				return done, err
			}
			done++
		}
	}
	if done > 0 {
		return done, m.St.Compact()
	}
	return 0, nil
}
