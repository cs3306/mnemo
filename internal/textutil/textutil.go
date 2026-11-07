// Package textutil holds small, dependency-free text helpers.
package textutil

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

var (
	thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)
	fenceRe = regexp.MustCompile("(?m)^```(?:json)?\\s*|\\s*```$")
)

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// EstTokens is a tokenizer-free estimate: CJK ~1 token/char, everything else ~3.5 chars/token.
func EstTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + int(float64(other)/3.5) + 1
}

// StripThink removes <think>…</think> blocks (and a dangling closing tag).
func StripThink(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// ExtractJSON parses JSON from a model reply, tolerating code fences and surrounding prose.
func ExtractJSON(s string, v any) bool {
	s = StripThink(s)
	s = strings.TrimSpace(fenceRe.ReplaceAllString(strings.TrimSpace(s), ""))
	if json.Unmarshal([]byte(s), v) == nil {
		return true
	}
	start := strings.IndexAny(s, "{[")
	end := strings.LastIndexAny(s, "}]")
	if start < 0 || end <= start {
		return false
	}
	return json.Unmarshal([]byte(s[start:end+1]), v) == nil
}

// Bigrams returns the set of character bigrams (works for CJK without a tokenizer).
func Bigrams(s string) map[string]struct{} {
	rs := []rune(strings.ToLower(strings.Join(strings.Fields(s), "")))
	out := make(map[string]struct{})
	if len(rs) == 0 {
		return out
	}
	if len(rs) == 1 {
		out[string(rs)] = struct{}{}
		return out
	}
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = struct{}{}
	}
	return out
}

// Overlap is |a∩b| / min(|a|,|b|).
func Overlap(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	n := 0
	for k := range a {
		if _, ok := b[k]; ok {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// SplitMessage splits text for Telegram's message limit, preferring line / sentence boundaries.
func SplitMessage(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{"…"}
	}
	rs := []rune(text)
	var out []string
	for len(rs) > limit {
		cut := limit
		for i := limit - 1; i > limit/2; i-- {
			if rs[i] == '\n' || rs[i] == '。' {
				cut = i + 1
				break
			}
		}
		if part := strings.TrimSpace(string(rs[:cut])); part != "" {
			out = append(out, part)
		}
		rs = []rune(strings.TrimSpace(string(rs[cut:])))
	}
	if len(rs) > 0 {
		out = append(out, string(rs))
	}
	return out
}
