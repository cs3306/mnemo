package bot

import (
	"math"
	"math/rand/v2"
	"regexp"
	"strings"
)

// Weighted voice replies (TTS_MODE=weighted). Each reply is spoken or typed by a dice roll whose odds start at
// TTS_VOICE_WEIGHT (typed message) or TTS_VOICE_WEIGHT_IN (voice note) and then move with the situation, the way a
// person's habit would: short and warm replies are more likely to be spoken, code and lists never are, a long run of
// voice messages makes the next one less likely, and so on. She can lean the odds with [[语音]] / [[文字]] at the
// start of a reply (a nudge, not an order), or simply call send_voice. What the user asks for always wins.

// Strength of each nudge, in logit units (+1 ≈ odds ×2.7). Edit and rebuild to taste.
const (
	voiceShort    = 0.8  // reply up to 40 characters
	voiceMedium   = 0.3  // up to 120
	voiceLongish  = -0.8 // beyond 60% of TTS_MAX_CHARS
	voiceWarm     = 1.0  // affectionate / greeting / playful wording
	voiceComfort  = 0.8  // the user sounds tired or down
	voiceTech     = -0.8 // technical talk
	voiceStructed = -4.0 // code, table, link or list: practically never
	voiceRunVoice = -0.7 // per voice reply in a row (up to 3)
	voiceRunText  = 0.35 // per typed reply in a row (up to 4): do not go silent for ever
	voiceLean     = 1.5  // her own [[语音]] / [[文字]]
	voiceJitter   = 0.6  // random ± shift per reply
)

var (
	warmRe    = regexp.MustCompile(`晚安|早安|午安|想你|抱抱|亲亲|宝贝|乖|辛苦了|加油|嘿嘿|哈哈|呜|哼|撒娇|爱你|心疼|么么|好呀|太好了|开心`)
	comfortRe = regexp.MustCompile(`难过|崩溃|好累|累死|累了|想哭|哭了|焦虑|睡不着|失眠|烦死|好烦|压力|害怕|不开心|委屈`)
	techRe    = regexp.MustCompile(`(?i)报错|代码|命令|配置|docker|ssh|https?:|函数|编译|定理|证明|矩阵|公式|\bgit\b|\bsudo\b`)
	listRe    = regexp.MustCompile(`(?m)^\s*(?:[-*•]|\d+[.、)])\s`)
	leanRe    = regexp.MustCompile(`^\s*\[\[\s*(语音|文字|voice|text)\s*\]\]\s*`)
)

// voiceCue is what the decision looks at.
type voiceCue struct {
	User, Reply string
	Streak      int // >0: that many voice replies in a row, <0: that many typed replies in a row
	Lean        int // +1 she leaned to voice, -1 to text
	Adapt       bool
}

func clampP(p float64) float64 { return math.Min(0.98, math.Max(0.02, p)) }

// voiceProb is the chance (0..1) that this reply is spoken. base is the configured weight; jitter is in [-1,1].
func voiceProb(base float64, c voiceCue, jitter float64) float64 {
	x := math.Log(clampP(base) / (1 - clampP(base)))
	x += float64(c.Lean) * voiceLean
	if c.Adapt {
		n := len([]rune(strings.TrimSpace(c.Reply)))
		switch {
		case strings.Contains(c.Reply, "```") || strings.Contains(c.Reply, "|---") || strings.Contains(c.Reply, "http") || len(listRe.FindAllString(c.Reply, 3)) >= 3:
			x += voiceStructed
		case n <= 40:
			x += voiceShort
		case n <= 120:
			x += voiceMedium
		}
		if warmRe.MatchString(c.Reply) || warmRe.MatchString(c.User) {
			x += voiceWarm
		}
		if comfortRe.MatchString(c.User) {
			x += voiceComfort
		}
		if techRe.MatchString(c.Reply) || techRe.MatchString(c.User) {
			x += voiceTech
		}
		switch {
		case c.Streak > 0:
			x += voiceRunVoice * float64(min(c.Streak, 3))
		case c.Streak < 0:
			x += voiceRunText * float64(min(-c.Streak, 4))
		}
		x += jitter * voiceJitter
	}
	return 1 / (1 + math.Exp(-x))
}

// stripLean removes a leading [[语音]] / [[文字]] and reports which way she leaned (0 when she did not).
func stripLean(s string) (string, int) {
	lean := 0
	for {
		m := leanRe.FindStringSubmatch(s)
		if m == nil {
			return s, lean
		}
		lean = 1
		if m[1] == "文字" || m[1] == "text" {
			lean = -1
		}
		s = s[len(m[0]):]
	}
}

const leanNote = "（系统说明，不要提及：这条回复是否发成语音由系统按概率随机决定，你可以影响但不能决定。" +
	"觉得这句话更适合说出来（撒娇、安慰、晚安、很短的回应），就在回复最开头写 [[语音]]；更适合看文字（代码、链接、列表、需要细看的内容），就写 [[文字]]。" +
	"这只是倾向，系统不一定采纳，标记不会显示给用户。想确保开口就调用 send_voice。用户明确要求语音时，系统一定会发语音。）"

var (
	askVoiceRe = regexp.MustCompile(`[发来录回用说讲][^，。,.!?！？\n]{0,4}语音|语音[^，。,.!?！？\n]{0,3}(回|发|说|讲|给我|答)|说(一|两|几)?句话|出个声|想听你|听听你的?声音|听你的声音|念给我听|读给我听|讲给我听|说给我听|念出来|读出来|开麦`)
	noVoiceRe  = regexp.MustCompile(`(别|不要|不用|不想|不必|无需|关掉|别再|先别)[^。！？\n]{0,3}语音|语音[^。！？\n]{0,2}(关|停)|打字(就行|回|吧)|用文字(回|就行)`)
)

// voiceAsk reads an explicit request in the user's message: +1 "speak to me", -1 "no voice", 0 nothing said.
// A refusal beats a request, so "别发语音" never triggers one.
func voiceAsk(text string) int {
	switch {
	case noVoiceRe.MatchString(text):
		return -1
	case askVoiceRe.MatchString(text):
		return 1
	}
	return 0
}

// notePlay records how the last reply of a chat went out, for the "run" adjustment.
func (a *App) notePlay(chat int64, voiced bool) {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	if a.vrun == nil {
		a.vrun = map[int64]int{}
	}
	s := a.vrun[chat]
	switch {
	case voiced && s > 0:
		s++
	case voiced:
		s = 1
	case s < 0:
		s--
	default:
		s = -1
	}
	a.vrun[chat] = s
}

func (a *App) streak(chat int64) int {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	return a.vrun[chat]
}

func (a *App) roll() float64 {
	if a.rnd != nil {
		return a.rnd()
	}
	return rand.Float64()
}

// leadForSpeech cuts text at the last sentence end within max runes, for a requested voice message whose full reply
// is too long to read out (the whole text is still sent).
func leadForSpeech(text string, max int) string {
	r := []rune(strings.TrimSpace(text))
	if max <= 0 || len(r) <= max {
		return string(r)
	}
	cut := -1
	for i := 0; i < max; i++ {
		switch r[i] {
		case '。', '！', '？', '.', '!', '?', '\n':
			cut = i + 1
		}
	}
	if cut < max/3 {
		cut = max
	}
	return strings.TrimSpace(string(r[:cut]))
}
