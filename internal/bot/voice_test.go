package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"companion/internal/telegram"
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
}

func (f *fakeTTS) Synthesize(_ context.Context, text, _, format string) ([]byte, error) {
	f.input = text
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
