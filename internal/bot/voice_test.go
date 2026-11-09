package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/telegram"
	"companion/internal/testutil"
)

type voiceTG struct {
	fakeTG
	vmu    sync.Mutex
	voices []string
}

func (f *voiceTG) SendVoice(_ context.Context, _ int64, name string, data []byte) error {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	f.voices = append(f.voices, name+":"+string(data))
	return nil
}

type fakeSTT struct {
	text string
	err  error
	name string
	lang string
}

func (f *fakeSTT) Transcribe(_ context.Context, _ []byte, name, language string) (string, error) {
	f.name, f.lang = name, language
	return f.text, f.err
}

type fakeTTS struct {
	err   error
	input string
	voice string
}

func (f *fakeTTS) Synthesize(_ context.Context, text, voice, format string) ([]byte, error) {
	f.input, f.voice = text, voice
	if f.err != nil {
		return nil, f.err
	}
	return []byte("AUDIO-" + format), nil
}

func voiceMsg(uid int64) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{
		Chat: telegram.Chat{ID: uid, Type: "private"}, From: &telegram.User{ID: uid},
		Voice: &telegram.Voice{FileID: "f1", MimeType: "audio/ogg"},
	}}
}

func voiceApp(t *testing.T) (*App, *voiceTG, *fakeSTT, *fakeTTS) {
	t.Helper()
	app, _, _, _ := newApp(t, "42")
	tg := &voiceTG{}
	app.tg = tg
	stt, tts := &fakeSTT{text: "帮我讲讲特征值"}, &fakeTTS{}
	app.stt, app.tts = stt, tts
	app.cfg.TTSText, app.cfg.TTSFormat, app.cfg.TTSMaxChars, app.cfg.STTLanguage = true, "mp3", 500, "zh"
	app.ttsMode.Store(ttsAuto)
	return app, tg, stt, tts
}

func TestVoiceInIsTranscribedAndAnsweredWithVoice(t *testing.T) {
	app, tg, stt, tts := voiceApp(t)
	app.Handle(context.Background(), voiceMsg(42))
	app.Wait()
	if len(tg.sent) < 2 || tg.sent[0] != "🎤 帮我讲讲特征值" || tg.sent[1] != "早呀，今天想学什么？" {
		t.Fatalf("sent=%v", tg.sent)
	}
	if stt.name != "voice.ogg" || stt.lang != "zh" {
		t.Fatalf("stt args %q %q", stt.name, stt.lang)
	}
	if len(tg.voices) != 1 || tg.voices[0] != "reply.mp3:AUDIO-mp3" || tts.input != "早呀，今天想学什么？" {
		t.Fatalf("voices=%v input=%q", tg.voices, tts.input)
	}
}

func TestTextInGetsVoiceOnlyWhenModeIsAlways(t *testing.T) {
	app, tg, _, _ := voiceApp(t)
	app.Handle(context.Background(), msg(42, "你好"))
	if len(tg.voices) != 0 {
		t.Fatal("auto mode must not speak to typed text")
	}
	app.Handle(context.Background(), msg(42, "/voice on"))
	app.Handle(context.Background(), msg(42, "你好"))
	app.Wait()
	if len(tg.voices) != 1 {
		t.Fatalf("voices=%v", tg.voices)
	}
	app.Handle(context.Background(), msg(42, "/voice off"))
	app.Handle(context.Background(), voiceMsg(42))
	app.Wait()
	if len(tg.voices) != 1 {
		t.Fatal("off must not speak")
	}
}

func TestVoiceOnlyReplyWhenTextDisabled(t *testing.T) {
	app, tg, _, _ := voiceApp(t)
	app.cfg.TTSText = false
	app.Handle(context.Background(), voiceMsg(42))
	app.Wait()
	for _, s := range tg.sent {
		if s == "早呀，今天想学什么？" {
			t.Fatalf("text must be skipped when the voice went out: %v", tg.sent)
		}
	}
	if len(tg.voices) != 1 {
		t.Fatalf("voices=%v", tg.voices)
	}
}

func TestSpeechFailureFallsBackToText(t *testing.T) {
	app, tg, _, tts := voiceApp(t)
	app.cfg.TTSText = false
	tts.err = errors.New("boom")
	app.Handle(context.Background(), voiceMsg(42))
	app.Wait()
	if len(tg.voices) != 0 || tg.last() != "早呀，今天想学什么？" {
		t.Fatalf("voices=%v sent=%v", tg.voices, tg.sent)
	}
}

func TestVoiceWithoutSTT(t *testing.T) {
	app, tg, _, _ := voiceApp(t)
	app.stt = nil
	app.Handle(context.Background(), voiceMsg(42))
	if !strings.Contains(tg.last(), "STT_ENABLED") {
		t.Fatalf("%v", tg.sent)
	}
}

func TestTranscriptStartingWithSlashIsNotACommand(t *testing.T) {
	app, tg, stt, _ := voiceApp(t)
	stt.text = "/forget 1"
	app.Handle(context.Background(), voiceMsg(42))
	app.Wait()
	if tg.sent[1] != "早呀，今天想学什么？" {
		t.Fatalf("a spoken slash must go to the model, not the command parser: %v", tg.sent)
	}
}

func TestEmptyAndFailedTranscription(t *testing.T) {
	app, tg, stt, _ := voiceApp(t)
	stt.text = ""
	app.Handle(context.Background(), voiceMsg(42))
	if !strings.Contains(tg.last(), "没听清") {
		t.Fatalf("%v", tg.sent)
	}
	stt.err = errors.New("down")
	app.Handle(context.Background(), voiceMsg(42))
	if !strings.Contains(tg.last(), "语音识别失败") {
		t.Fatalf("%v", tg.sent)
	}
}

func voiceToolApp(t *testing.T, script ...llm.Result) (*App, *voiceTG, *fakeTTS, *testutil.ToolChat) {
	t.Helper()
	app, tg, _, tts := voiceApp(t)
	tc := &testutil.ToolChat{Script: script}
	app.chat = tc
	return app, tg, tts, tc
}

func sendVoiceCall(text string) json.RawMessage {
	args, _ := json.Marshal(map[string]string{"text": text})
	b, _ := json.Marshal([]map[string]any{{"id": "c1", "type": "function",
		"function": map[string]any{"name": "send_voice", "arguments": string(args)}}})
	return b
}

func TestSendVoiceToolIsOfferedAndSpeaks(t *testing.T) {
	app, tg, tts, tc := voiceToolApp(t,
		llm.Result{ToolCalls: sendVoiceCall("晚安，早点睡")},
		llm.Result{Text: ""})
	app.Handle(context.Background(), msg(42, "我要睡了"))
	app.Wait()
	if !strings.Contains(string(tc.ToolsSeen[0]), `"send_voice"`) {
		t.Fatalf("tool not offered: %s", tc.ToolsSeen[0])
	}
	if len(tg.voices) != 1 || tts.input != "晚安，早点睡" {
		t.Fatalf("voices=%v input=%q", tg.voices, tts.input)
	}
	for _, s := range tg.sent {
		if strings.Contains(s, "出错了") {
			t.Fatalf("voice-only turn must not error: %v", tg.sent)
		}
	}
}

func TestSendVoiceNotOfferedWithoutTTS(t *testing.T) {
	app, _, _, tc := voiceToolApp(t, llm.Result{Text: "好的"})
	app.tts = nil
	app.Handle(context.Background(), msg(42, "你好"))
	app.Wait()
	if len(tc.ToolsSeen) > 0 && strings.Contains(string(tc.ToolsSeen[0]), "send_voice") {
		t.Fatal("must not offer send_voice without TTS")
	}
}

func TestSendVoiceFailureIsReportedToTheModel(t *testing.T) {
	app, tg, tts, tc := voiceToolApp(t,
		llm.Result{ToolCalls: sendVoiceCall("你好呀")},
		llm.Result{Text: "语音没成功，文字发你"})
	tts.err = errors.New("boom")
	app.Handle(context.Background(), msg(42, "说句话"))
	app.Wait()
	last := tc.Calls[len(tc.Calls)-1]
	if got := last[len(last)-1]; got.Role != "tool" || !strings.Contains(fmt.Sprint(got.Content), "没发出去") {
		t.Fatalf("model must be told it failed: %+v", got)
	}
	if len(tg.voices) != 0 || tg.last() != "语音没成功，文字发你" {
		t.Fatalf("voices=%v sent=%v", tg.voices, tg.sent)
	}
}

func TestToolVoiceSuppressesAutoReadAloud(t *testing.T) {
	app, tg, _, _ := voiceToolApp(t,
		llm.Result{ToolCalls: sendVoiceCall("来啦")},
		llm.Result{Text: "刚给你发了语音"})
	app.Handle(context.Background(), voiceMsg(42)) // voice in -> auto mode would also read the reply aloud
	app.Wait()
	if len(tg.voices) != 1 {
		t.Fatalf("exactly the tool's voice, not a second read-aloud: %v", tg.voices)
	}
}

func sendVoiceCallAs(text, voice string) json.RawMessage {
	args, _ := json.Marshal(map[string]string{"text": text, "voice": voice})
	b, _ := json.Marshal([]map[string]any{{"id": "c1", "type": "function",
		"function": map[string]any{"name": "send_voice", "arguments": string(args)}}})
	return b
}

func TestSendVoiceLetsHerPickAVoice(t *testing.T) {
	app, _, tts, tc := voiceToolApp(t,
		llm.Result{ToolCalls: sendVoiceCall("先用默认的")},
		llm.Result{Text: ""})
	app.cfg.TTSVoice = "mll"
	app.cfg.TTSVoices = []config.TTSVoice{{Name: "mll", Desc: "软萌日常"}, {Name: "serena", Desc: "更俏皮夸张"}}
	app.Handle(context.Background(), msg(42, "说句话"))
	app.Wait()
	def := string(tc.ToolsSeen[0])
	if !strings.Contains(def, `"enum":["mll","serena"]`) || !strings.Contains(def, "更俏皮夸张") {
		t.Fatalf("voice choice not offered: %s", def)
	}
	if tts.voice != "mll" {
		t.Fatalf("default voice not used: %q", tts.voice)
	}

	app, _, tts, _ = voiceToolApp(t, llm.Result{ToolCalls: sendVoiceCallAs("嘿嘿", "serena")}, llm.Result{Text: ""})
	app.cfg.TTSVoice = "mll"
	app.cfg.TTSVoices = []config.TTSVoice{{Name: "mll"}, {Name: "serena"}}
	app.Handle(context.Background(), msg(42, "来一句"))
	app.Wait()
	if tts.voice != "serena" {
		t.Fatalf("chosen voice not used: %q", tts.voice)
	}

	app, _, tts, _ = voiceToolApp(t, llm.Result{ToolCalls: sendVoiceCallAs("嘿嘿", "evil; rm")}, llm.Result{Text: ""})
	app.cfg.TTSVoice = "mll"
	app.cfg.TTSVoices = []config.TTSVoice{{Name: "mll"}, {Name: "serena"}}
	app.Handle(context.Background(), msg(42, "来一句"))
	app.Wait()
	if tts.voice != "mll" {
		t.Fatalf("unknown voice must fall back to the default: %q", tts.voice)
	}
}

func TestSingleVoiceHasNoVoiceParameter(t *testing.T) {
	app, _, _, tc := voiceToolApp(t, llm.Result{Text: "好"})
	app.cfg.TTSVoice = "mll"
	app.Handle(context.Background(), msg(42, "你好"))
	app.Wait()
	if strings.Contains(string(tc.ToolsSeen[0]), `"voice"`) {
		t.Fatalf("one voice needs no choice: %s", tc.ToolsSeen[0])
	}
}
