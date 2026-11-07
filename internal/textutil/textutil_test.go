package textutil

import (
	"strings"
	"testing"
)

func TestStripThink(t *testing.T) {
	if got := StripThink("<think>hmm\nx</think>你好"); got != "你好" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON(t *testing.T) {
	var v struct{ A int }
	if !ExtractJSON("here:\n```json\n{\"A\": 3}\n```", &v) || v.A != 3 {
		t.Fatalf("fenced json not parsed: %+v", v)
	}
	if ExtractJSON("no json here", &v) {
		t.Fatal("should fail")
	}
}

func TestSplitMessage(t *testing.T) {
	text := strings.Repeat("你好世界。", 1000)
	parts := SplitMessage(text, 3500)
	if len(parts) < 2 {
		t.Fatal("expected split")
	}
	if strings.Join(parts, "") != text {
		t.Fatal("split lost content")
	}
	for _, p := range parts {
		if len([]rune(p)) > 3500 {
			t.Fatal("part too long")
		}
	}
}

func TestOverlap(t *testing.T) {
	if Overlap(Bigrams("线性代数作业"), Bigrams("线性代数")) <= Overlap(Bigrams("线性代数作业"), Bigrams("今天吃饭")) {
		t.Fatal("overlap ordering wrong")
	}
}
