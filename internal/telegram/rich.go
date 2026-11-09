package telegram

import (
	"regexp"
	"strings"
	"unicode"
)

// PrepareRich adapts model output (CommonMark-ish Markdown with LaTeX) to Telegram's rich-message Markdown.
// Telegram's parser differs from CommonMark in ways that bite chat text, so three things are fixed up:
//
//  1. "$" that is not math (prices) is escaped, because "$...$" is inline math there.
//  2. A table that directly follows a paragraph line gets a blank line first, or it shows as raw pipes.
//  3. A single newline between two plain text lines becomes a blank line (a paragraph break). Soft line
//     breaks are not reliably kept, and a paragraph break is the one separator every Markdown parser honours.
//
// Fenced code and "$$" math blocks pass through untouched.
func PrepareRich(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines)+8)
	var (
		inFence bool
		fence   string
		inMath  bool
		prev    string // previous emitted line
	)
	emit := func(l string) {
		out = append(out, l)
		prev = l
	}
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		switch {
		case inFence:
			emit(line)
			if strings.HasPrefix(trim, fence) {
				inFence = false
			}
			continue
		case inMath:
			emit(line)
			if strings.Contains(trim, "$$") {
				inMath = false
			}
			continue
		}
		if f := fenceStart(trim); f != "" {
			if strings.TrimSpace(prev) != "" && isPlain(prev) {
				emit("")
			}
			inFence, fence = true, f
			emit(line)
			continue
		}
		if strings.HasPrefix(trim, "$$") {
			// A block that opens and closes on the same line ("$$x$$") stays one line.
			if !(len(trim) > 2 && strings.HasSuffix(trim, "$$")) {
				inMath = true
			}
			if strings.TrimSpace(prev) != "" && isPlain(prev) {
				emit("")
			}
			emit(line)
			continue
		}
		line = escapeDollars(line)
		trim = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "|"):
			if strings.TrimSpace(prev) != "" && !strings.HasPrefix(strings.TrimSpace(prev), "|") {
				emit("")
			}
		case trim != "" && isPlain(line) && strings.TrimSpace(prev) != "" && isPlain(prev):
			emit("")
		}
		emit(line)
	}
	return strings.Join(out, "\n")
}

var (
	listRe  = regexp.MustCompile(`^([-*+]|\d+[.)])\s`)
	ruleRe  = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
	mathHit = regexp.MustCompile(`[\\^_{}=<>+*/|]|[A-Za-z]`)
)

func fenceStart(trim string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trim, f) {
			return f
		}
	}
	return ""
}

// isPlain reports whether a line is ordinary paragraph text (not a heading, quote, list item, table row,
// rule, indented continuation, fence or math block, and not blank).
func isPlain(line string) bool {
	if line == "" || strings.TrimSpace(line) == "" {
		return false
	}
	if line[0] == ' ' || line[0] == '\t' {
		return false
	}
	t := strings.TrimSpace(line)
	switch t[0] {
	case '#', '>', '|':
		return false
	}
	if listRe.MatchString(t) || ruleRe.MatchString(t) || fenceStart(t) != "" || strings.HasPrefix(t, "$$") {
		return false
	}
	return true
}

// escapeDollars escapes "$" that do not delimit a formula. A span "$...$" is kept as math when the opening
// "$" is followed by a non-space, the closing "$" is preceded by a non-space and not followed by a digit,
// and (when it starts with a digit) the content looks like math rather than a price such as "$5.00,".
// Inline code spans and already-escaped "\$" are left alone; formulas must close on the same line.
func escapeDollars(line string) string {
	if !strings.Contains(line, "$") {
		return line
	}
	rs := []rune(line)
	var b strings.Builder
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			b.WriteRune(r)
			b.WriteRune(rs[i+1])
			i += 2
		case r == '`':
			j := i
			for j < len(rs) && rs[j] == '`' {
				j++
			}
			ticks := string(rs[i:j])
			if end := strings.Index(string(rs[j:]), ticks); end >= 0 {
				n := len([]rune(string(rs[j:])[:end])) + len(ticks)
				b.WriteString(string(rs[i : j+n]))
				i = j + n
			} else {
				b.WriteString(ticks)
				i = j
			}
		case r == '$' && i+1 < len(rs) && rs[i+1] == '$':
			if end := indexRunes(rs, "$$", i+2); end >= 0 {
				b.WriteString(string(rs[i : end+2]))
				i = end + 2
			} else {
				b.WriteString(`\$\$`)
				i += 2
			}
		case r == '$':
			if end := mathEnd(rs, i); end >= 0 {
				b.WriteString(string(rs[i : end+1]))
				i = end + 1
			} else {
				b.WriteString(`\$`)
				i++
			}
		default:
			b.WriteRune(r)
			i++
		}
	}
	return b.String()
}

func indexRunes(rs []rune, sub string, from int) int {
	pat := []rune(sub)
	for i := from; i+len(pat) <= len(rs); i++ {
		match := true
		for k := range pat {
			if rs[i+k] != pat[k] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// mathEnd returns the index of the "$" closing the formula opened at rs[i], or -1 if it is not math.
func mathEnd(rs []rune, i int) int {
	if i+1 >= len(rs) || unicode.IsSpace(rs[i+1]) {
		return -1
	}
	for k := i + 1; k < len(rs); k++ {
		if rs[k] == '\\' {
			k++
			continue
		}
		if rs[k] != '$' {
			continue
		}
		if unicode.IsSpace(rs[k-1]) || (k+1 < len(rs) && unicode.IsDigit(rs[k+1])) {
			continue
		}
		if unicode.IsDigit(rs[i+1]) && !mathHit.MatchString(string(rs[i+1:k])) {
			return -1
		}
		return k
	}
	return -1
}
