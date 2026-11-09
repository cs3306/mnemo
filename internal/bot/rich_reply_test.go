package bot

import (
	"context"
	"testing"

	"companion/internal/config"
)

type richTG struct {
	fakeTG
	rich []string
}

func (f *richTG) SendRich(_ context.Context, _ int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rich = append(f.rich, text)
	return nil
}

func TestSendReplyUsesRichOnlyForModelText(t *testing.T) {
	app, _, _, _ := newApp(t, "1")
	tg := &richTG{}
	app.tg = tg

	app.sendReply(context.Background(), 1, "模型回复 $x^2$")
	app.send(context.Background(), 1, "系统提示")

	if len(tg.rich) != 1 || tg.rich[0] != "模型回复 $x^2$" {
		t.Fatalf("model text must go out as a rich message: %v", tg.rich)
	}
	if len(tg.sent) != 1 || tg.sent[0] != "系统提示" {
		t.Fatalf("status/error text must stay plain: %v", tg.sent)
	}
}

func TestSendReplyHonoursTelegramRichOff(t *testing.T) {
	app, _, _, _ := newApp(t, "1")
	tg := &richTG{}
	app.tg = tg
	cfg, err := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": t.TempDir(), "TELEGRAM_RICH": "0"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	app.cfg = cfg

	app.sendReply(context.Background(), 1, "hi")
	if len(tg.rich) != 0 || len(tg.sent) != 1 {
		t.Fatalf("rich=%v plain=%v", tg.rich, tg.sent)
	}
}

func TestSendReplyFallsBackWhenClientHasNoRichSupport(t *testing.T) {
	app, tg, _, _ := newApp(t, "1") // fakeTG does not implement RichSender
	app.sendReply(context.Background(), 1, "hi")
	if tg.last() != "hi" {
		t.Fatalf("%q", tg.last())
	}
}
