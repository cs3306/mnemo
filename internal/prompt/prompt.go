// Package prompt assembles the model input. The layout is chosen for prefix-cache friendliness:
//
//	system  = persona (static) + style notes (changes at most daily) + core profile (rarely changes) + style anchors + rolling summary
//	history = recent un-summarised messages, stored verbatim (stable prefix between turns)
//	last    = [retrieved memories][learning progress][time] + the user's new message (varies per turn)
package prompt

import (
	"fmt"
	"strings"
	"time"

	"companion/internal/llm"
	"companion/internal/store"
	"companion/internal/textutil"
)

const DefaultPersona = "你是用户的 AI 伴侣和学习助手。语气温柔、耐心、自然。"

type Input struct {
	Persona      string
	Style        string // learned speaking-style notes (style.json), placed right after the persona
	Core         string
	Summary      string
	Pins         []string
	Recent       []store.Message // un-summarised history, oldest first, EXCLUDING the current message
	RecentBudget int             // token budget for Recent
	Memories     []store.Memory
	Learning     []store.Learning
	Now          time.Time
	UserText     string
}

func FormatMemories(mems []store.Memory, loc *time.Location) string {
	var b strings.Builder
	for _, m := range mems {
		day := time.Unix(int64(m.Created), 0).In(loc).Format("2006-01-02")
		fmt.Fprintf(&b, "- (%s) %s\n", day, m.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func FormatLearning(ls []store.Learning) string {
	names := map[string]string{"weak": "薄弱", "learning": "学习中", "solid": "已掌握"}
	var b strings.Builder
	for _, l := range ls {
		name := names[l.Status]
		if name == "" {
			name = l.Status
		}
		fmt.Fprintf(&b, "- %s（%s）", l.Topic, name)
		if l.ErrorPattern != "" {
			fmt.Fprintf(&b, "，常见错误：%s", l.ErrorPattern)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func Build(in Input) []llm.Message {
	persona := strings.TrimSpace(in.Persona)
	if persona == "" {
		persona = DefaultPersona
	}
	var sys strings.Builder
	sys.WriteString(persona)
	if st := strings.TrimSpace(in.Style); st != "" {
		sys.WriteString("\n\n## 说话风格笔记（从和他的相处里总结出来的；和上面的人设冲突时，以人设为准）\n" + st)
	}
	if in.Core != "" {
		sys.WriteString("\n\n## 关于用户的核心档案\n" + in.Core)
	}
	if len(in.Pins) > 0 {
		sys.WriteString("\n\n## 说话风格示例（你过去说过、用户喜欢的话；保持这种语气，不要照抄内容）")
		for i, p := range in.Pins {
			fmt.Fprintf(&sys, "\n%d. %s", i+1, p)
		}
	}
	if in.Summary != "" {
		sys.WriteString("\n\n## 此前对话的摘要\n" + in.Summary)
	}

	// newest-first until the budget is spent, then restore chronological order
	var hist []llm.Message
	used := 0
	for i := len(in.Recent) - 1; i >= 0; i-- {
		t := textutil.EstTokens(in.Recent[i].Content)
		if len(hist) > 0 && used+t > in.RecentBudget {
			break
		}
		hist = append(hist, llm.Text(in.Recent[i].Role, in.Recent[i].Content))
		used += t
	}
	for i, j := 0, len(hist)-1; i < j; i, j = i+1, j-1 {
		hist[i], hist[j] = hist[j], hist[i]
	}

	var blocks []string
	if len(in.Memories) > 0 {
		blocks = append(blocks, "[相关记忆]\n"+FormatMemories(in.Memories, in.Now.Location())+"\n[/相关记忆]")
	}
	if len(in.Learning) > 0 {
		blocks = append(blocks, "[学习进度]\n"+FormatLearning(in.Learning)+"\n[/学习进度]")
	}
	blocks = append(blocks, fmt.Sprintf("[当前时间：%s %s，时区 %s（UTC%s）。这是用户所在地的时间，不要凭语言猜时区，也不要为了查时间去联网搜索]",
		in.Now.Format("2006-01-02 15:04"), in.Now.Weekday(), in.Now.Location(), in.Now.Format("-07:00")))
	current := strings.Join(blocks, "\n") + "\n\n" + in.UserText

	msgs := make([]llm.Message, 0, len(hist)+2)
	msgs = append(msgs, llm.Text("system", sys.String()))
	msgs = append(msgs, hist...)
	return append(msgs, llm.Text("user", current))
}
