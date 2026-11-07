package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"companion/internal/llm"
	"companion/internal/store"
)

func text(m llm.Message) string { s, _ := m.Content.(string); return s }

func TestLayoutIsCacheFriendly(t *testing.T) {
	var recent []store.Message
	for i := 0; i < 200; i++ {
		recent = append(recent, store.Message{ID: int64(i + 1), Role: []string{"user", "assistant"}[i%2], Content: strings.Repeat("字", 50)})
	}
	in := Input{Persona: "PERSONA", Core: "CORE", Summary: "SUMMARY", Pins: []string{"PIN1"},
		Recent: recent, RecentBudget: 500, Memories: []store.Memory{{Text: "MEM1", Kind: "fact", Importance: 3}},
		Now: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC), UserText: "现在的问题"}
	msgs := Build(in)
	sys := text(msgs[0])
	for _, w := range []string{"PERSONA", "CORE", "SUMMARY", "PIN1"} {
		if !strings.Contains(sys, w) {
			t.Fatalf("system prompt missing %s", w)
		}
	}
	if strings.Contains(sys, "MEM1") {
		t.Fatal("volatile memories must not be in the system prompt (breaks prefix caching)")
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" || !strings.Contains(text(last), "MEM1") || !strings.Contains(text(last), "现在的问题") {
		t.Fatalf("last message wrong: %q", text(last))
	}
	if n := len(msgs); n >= 60 {
		t.Fatalf("history not trimmed to budget: %d messages", n)
	}
	// alternating-order sanity: newest history kept
	if !strings.Contains(fmt.Sprint(msgs[len(msgs)-2].Content), "字") {
		t.Fatal("expected recent history before the last message")
	}
}

func TestCurrentTimeStatesTheZone(t *testing.T) {
	loc, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		t.Skip("no tzdata")
	}
	msgs := Build(Input{Persona: "P", Now: time.Date(2026, 11, 7, 15, 24, 0, 0, loc), UserText: "hi"})
	last := text(msgs[len(msgs)-1])
	for _, w := range []string{"2026-11-07 15:24", "Saturday", "America/Vancouver", "UTC-0"} {
		if !strings.Contains(last, w) {
			t.Fatalf("time block missing %q:\n%s", w, last)
		}
	}
}
