package bot

import (
	"fmt"
	"regexp"
	"strings"
)

// voiceNameRe matches a leading [[声:name]] / [[声音：name]] / [[voice:name]]: which TTS voice she wants for this reply.
var voiceNameRe = regexp.MustCompile(`^\s*\[\[\s*(?:声音?|voice)\s*[:：]\s*([^\]\s]+?)\s*\]\]\s*`)

// voiceNames lists the voices she may choose between (empty when fewer than two are configured).
func (a *App) voiceNames() []string {
	if len(a.cfg.TTSVoices) < 2 {
		return nil
	}
	names := make([]string, 0, len(a.cfg.TTSVoices))
	for _, v := range a.cfg.TTSVoices {
		names = append(names, v.Name)
	}
	return names
}

// stripVoiceName removes a leading voice marker and returns the configured voice it named ("" when the name is unknown
// or none was given). The marker is stripped even when the name is unknown, so it never reaches the user.
func stripVoiceName(s string, names []string) (string, string) {
	m := voiceNameRe.FindStringSubmatch(s)
	if m == nil {
		return s, ""
	}
	s = s[len(m[0]):]
	for _, n := range names {
		if strings.EqualFold(n, m[1]) {
			return s, n
		}
	}
	return s, ""
}

// voiceChoiceNote tells her which voices exist and how to pick one for an automatic voice reply.
func (a *App) voiceChoiceNote() string {
	var b strings.Builder
	for _, v := range a.cfg.TTSVoices {
		b.WriteString(v.Name)
		if v.Desc != "" {
			fmt.Fprintf(&b, "（%s）", v.Desc)
		}
		b.WriteString("；")
	}
	return "（系统说明，不要提及：你有多种声音：" + b.String() + "这条回复如果被发成语音，默认用 " + a.cfg.TTSVoice +
		"。想换声音，就在回复最开头写 [[声:名字]]（例如 [[声:" + a.cfg.TTSVoices[len(a.cfg.TTSVoices)-1].Name +
		"]]），按这句话的语气自己选；不写就用默认。标记不会显示给用户；调用 send_voice 时用它的 voice 参数。）"
}
