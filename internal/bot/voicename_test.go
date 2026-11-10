package bot

import (
	"context"
	"testing"

	"companion/internal/config"
	"companion/internal/testutil"
)

func TestAutomaticVoiceReplyUsesTheVoiceSheChose(t *testing.T) {
	for _, c := range []struct{ reply, sent, voice string }{
		{"[[声:serena]] 哎呀讨厌", "哎呀讨厌", "serena"},
		{"[[语音]][[声:Serena]] 哎呀讨厌", "哎呀讨厌", "serena"},
		{"[[声:nobody]] 哎呀讨厌", "哎呀讨厌", "mll"},
		{"哎呀讨厌", "哎呀讨厌", "mll"},
	} {
		app, tg, _, tts := voiceApp(t)
		app.chat.(*testutil.Chat).Replies = []string{c.reply}
		app.cfg.TTSVoice = "mll"
		app.cfg.TTSVoices = []config.TTSVoice{{Name: "mll"}, {Name: "serena", Desc: "撒娇"}}
		app.Handle(context.Background(), msg(42, "/voice on"))
		app.Handle(context.Background(), msg(42, "你好"))
		app.Wait()
		if tts.voice != c.voice || tg.last() != c.sent {
			t.Errorf("%q: voice=%q sent=%q; want %q / %q", c.reply, tts.voice, tg.last(), c.voice, c.sent)
		}
	}
}

func TestStripVoiceName(t *testing.T) {
	names := []string{"mll", "serena"}
	cases := []struct{ in, rest, voice string }{
		{"[[声:serena]] 哎呀讨厌", "哎呀讨厌", "serena"},
		{"[[声音：Serena]]\n晚安", "晚安", "serena"},
		{"[[voice: mll ]]好", "好", "mll"},
		{"[[声:nobody]] 嗯", "嗯", ""},
		{"没有标记", "没有标记", ""},
		{"正文里的 [[声:serena]] 不动", "正文里的 [[声:serena]] 不动", ""},
	}
	for _, c := range cases {
		rest, v := stripVoiceName(c.in, names)
		if rest != c.rest || v != c.voice {
			t.Errorf("stripVoiceName(%q) = %q, %q; want %q, %q", c.in, rest, v, c.rest, c.voice)
		}
	}
}

func TestStripBothMarkersEitherOrder(t *testing.T) {
	names := []string{"mll", "serena"}
	for _, in := range []string{"[[声:serena]][[语音]] 嘿嘿", "[[语音]] [[声:serena]] 嘿嘿"} {
		s, v := stripVoiceName(in, names)
		s, lean := stripLean(s)
		if v == "" {
			s, v = stripVoiceName(s, names)
		}
		if s != "嘿嘿" || v != "serena" || lean != 1 {
			t.Errorf("%q -> %q voice=%q lean=%d", in, s, v, lean)
		}
	}
}
