package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"strings"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/store"
	"companion/internal/testutil"
)

// errChat fails every call.
type errChat struct{}

func (errChat) Complete(context.Context, []llm.Message, float64) (string, error) {
	return "", errors.New("boom")
}

func visionApp(t *testing.T, vision llm.Completer, replies ...string) (*App, *fakeTG) {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": dir, "ALLOWED_USER_IDS": "42", "LLM_MODEL": "m", "VISION_ENABLED": "true"}[k]
	})
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := &fakeTG{}
	chat := &testutil.Chat{Replies: replies}
	app, err := New(cfg, Deps{Store: st, Chat: chat, Maint: &testutil.Chat{}, Vision: vision, Embed: &testutil.Embed{Name: "e"}, TG: tg, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	app.probeCode = func() string { return "4821" }
	return app, tg
}

func TestProbeImageIsAValidPNGWithInk(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(probeImage("4821")))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 27*16 || b.Dy() != 11*16 {
		t.Fatalf("unexpected size %v", b)
	}
	black := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, _, _, _ := img.At(x, y).RGBA(); r == 0 {
				black++
			}
		}
	}
	if black < 500 {
		t.Fatalf("image looks blank (%d black pixels)", black)
	}
	if bytes.Equal(probeImage("4821"), probeImage("1234")) {
		t.Fatal("different codes must render differently")
	}
}

func TestProbePassesWhenModelReadsDigits(t *testing.T) {
	app, _ := visionApp(t, &testutil.Chat{Replies: []string{"4821"}}, "好")
	if err := app.probeVision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.visionState.Load() != visionOK {
		t.Fatal("state should be OK")
	}
	// the request really carried an image
	v := app.vision.(*testutil.Chat)
	if len(v.Calls) != 1 || !strings.Contains(llmDump(v.Calls[0]), "data:image/png;base64,") {
		t.Fatalf("probe did not send a PNG image: %s", llmDump(v.Calls[0]))
	}
}

func TestProbeFailureBlocksImageReadingUntilFixed(t *testing.T) {
	// a model that cannot see the image invents a transcription
	v := &testutil.Chat{Replies: []string{"设 H 是 Hilbert 空间，Φ(A)^2 ≤ ‖Φ‖Φ(A²)"}}
	app, tg := visionApp(t, v, "好")
	err := app.probeVision(context.Background())
	if !errors.Is(err, errVisionBad) {
		t.Fatalf("want errVisionBad, got %v", err)
	}
	if _, derr := app.describeImages(context.Background(), []Image{{Data: []byte("x"), Mime: "image/png"}}); !errors.Is(derr, errVisionBad) {
		t.Fatalf("image reading must be blocked, got %v", derr)
	}
	if len(v.Calls) != 1 {
		t.Fatalf("blocked reading must not call the model again; calls=%d", len(v.Calls))
	}
	// /visiontest reports it, and a fixed backend clears the block
	app.command(context.Background(), 42, "/visiontest")
	if !strings.Contains(tg.last(), "未通过") {
		t.Fatalf("got %q", tg.last())
	}
	v.Replies = []string{"4821"}
	v.Calls = nil
	app.command(context.Background(), 42, "/visiontest")
	if !strings.Contains(tg.last(), "通过") || strings.Contains(tg.last(), "未通过") {
		t.Fatalf("got %q", tg.last())
	}
	if _, derr := app.describeImages(context.Background(), []Image{{Data: []byte("x"), Mime: "image/png"}}); derr != nil {
		t.Fatalf("block should be cleared: %v", derr)
	}
}

func TestProbeRequestFailureDoesNotBlock(t *testing.T) {
	app, _ := visionApp(t, errChat{}, "好")
	err := app.probeVision(context.Background())
	if err == nil || errors.Is(err, errVisionBad) {
		t.Fatalf("want a plain request error, got %v", err)
	}
	if app.visionState.Load() != visionUnknown {
		t.Fatal("a network error must not mark the model bad")
	}
}

func TestVisionTestWithoutVision(t *testing.T) {
	app, tg, _, _ := newApp(t, "42")
	app.command(context.Background(), 42, "/visiontest")
	if !strings.Contains(tg.last(), "读图") {
		t.Fatalf("got %q", tg.last())
	}
}

func TestProbeConfigDefault(t *testing.T) {
	cfg, _ := config.Load(func(k string) string { return map[string]string{"VISION_ENABLED": "true"}[k] })
	if !cfg.VisionProbe {
		t.Fatal("VISION_PROBE should default to true")
	}
	cfg, _ = config.Load(func(k string) string {
		return map[string]string{"VISION_ENABLED": "true", "VISION_PROBE": "false"}[k]
	})
	if cfg.VisionProbe {
		t.Fatal("VISION_PROBE=false should disable it")
	}
}

func llmDump(msgs []llm.Message) string {
	b, _ := json.Marshal(msgs)
	return string(b)
}

func TestRunProbesVisionAtStartup(t *testing.T) {
	app, _ := visionApp(t, &testutil.Chat{Replies: []string{"完全不相干的编造内容"}}, "好")
	app.cfg.VisionProbe = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = app.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for app.visionState.Load() == visionUnknown && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if app.visionState.Load() != visionBad {
		t.Fatal("startup probe should have marked the model bad")
	}
}
