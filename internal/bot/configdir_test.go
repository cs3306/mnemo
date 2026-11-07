package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"companion/internal/assets"
	"companion/internal/config"
	"companion/internal/store"
	"companion/internal/testutil"
)

// cfgApp builds an App whose persona.md / profile.md live in a CONFIG_DIR separate from DATA_DIR.
func cfgApp(t *testing.T) (*App, *fakeTG, *testutil.Chat, *store.Store, string) {
	t.Helper()
	data, cfgDir := t.TempDir(), t.TempDir()
	cfg, err := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": data, "CONFIG_DIR": cfgDir, "ALLOWED_USER_IDS": "42", "LLM_MODEL": "m", "EXTRACT_EVERY": "100"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := &fakeTG{}
	chat := &testutil.Chat{Replies: []string{"好"}}
	app, err := New(cfg, Deps{Store: st, Chat: chat, Maint: &testutil.Chat{}, Embed: &testutil.Embed{Name: "e"}, TG: tg, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return app, tg, chat, st, cfgDir
}

func systemOf(chat *testutil.Chat, call int) string {
	return fmt.Sprint(chat.Calls[call][0].Content)
}

func TestConfigDirIsNeverWrittenAndDefaultsApply(t *testing.T) {
	app, _, chat, _, cfgDir := cfgApp(t)
	if ents, _ := os.ReadDir(cfgDir); len(ents) != 0 {
		t.Fatalf("the app must not write into CONFIG_DIR, found %d entries", len(ents))
	}
	if _, err := app.Respond(context.Background(), 42, "你好", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(systemOf(chat, 0), strings.TrimSpace(assets.PersonaExample)[:30]) {
		t.Fatal("a missing persona.md should fall back to the built-in example persona")
	}
}

func TestPersonaEditsApplyOnTheNextTurn(t *testing.T) {
	app, _, chat, _, cfgDir := cfgApp(t)
	p := filepath.Join(cfgDir, "persona.md")
	for i, want := range []string{"PERSONA-ONE", "PERSONA-TWO"} {
		if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Respond(context.Background(), 42, fmt.Sprintf("消息%d", i), nil); err != nil {
			t.Fatal(err)
		}
		if got := systemOf(chat, i); !strings.Contains(got, want) {
			t.Fatalf("turn %d: persona %q not in system prompt", i, want)
		}
	}
}

func TestUnreadablePersonaFallsBackWithoutFailing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	app, _, chat, _, cfgDir := cfgApp(t)
	p := filepath.Join(cfgDir, "persona.md")
	if err := os.WriteFile(p, []byte("SECRET-PERSONA"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Respond(context.Background(), 42, "你好", nil); err != nil {
		t.Fatalf("an unreadable persona.md must not fail the turn: %v", err)
	}
	if strings.Contains(systemOf(chat, 0), "SECRET-PERSONA") || !strings.Contains(systemOf(chat, 0), strings.TrimSpace(assets.PersonaExample)[:30]) {
		t.Fatal("should use the built-in persona when the file cannot be read")
	}
}

func TestProfileFileSeedsAndReloads(t *testing.T) {
	data, cfgDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "profile.md"), []byte("称呼：小明\n在做的事：线代"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(func(k string) string {
		return map[string]string{"DATA_DIR": data, "CONFIG_DIR": cfgDir, "ALLOWED_USER_IDS": "42", "LLM_MODEL": "m"}[k]
	})
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := &fakeTG{}
	app, err := New(cfg, Deps{Store: st, Chat: &testutil.Chat{}, Maint: &testutil.Chat{}, Embed: &testutil.Embed{Name: "e"}, TG: tg, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Core(), "小明") {
		t.Fatalf("an edited profile.md should seed the core profile, got %q", st.Core())
	}
	// edit the file, reload
	if err := os.WriteFile(filepath.Join(cfgDir, "profile.md"), []byte("称呼：阿杰"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.command(context.Background(), 42, "/profile reload")
	if !strings.Contains(st.Core(), "阿杰") || strings.Contains(st.Core(), "小明") {
		t.Fatalf("reload should replace the core profile, got %q", st.Core())
	}
	// the untouched placeholder is refused
	if err := os.WriteFile(filepath.Join(cfgDir, "profile.md"), []byte(assets.ProfileExample), 0o644); err != nil {
		t.Fatal(err)
	}
	app.command(context.Background(), 42, "/profile reload")
	if !strings.Contains(tg.last(), "空白模板") || !strings.Contains(st.Core(), "阿杰") {
		t.Fatalf("placeholder must not overwrite the profile; reply %q core %q", tg.last(), st.Core())
	}
	// missing file
	os.Remove(filepath.Join(cfgDir, "profile.md"))
	app.command(context.Background(), 42, "/profile reload")
	if !strings.Contains(tg.last(), "失败") {
		t.Fatalf("got %q", tg.last())
	}
}

func TestDefaultConfigDirIsDataDirAndSeedsExamples(t *testing.T) {
	app, _, _, _ := newApp(t, "42")
	if app.cfg.ConfigDir != app.cfg.DataDir {
		t.Fatal("CONFIG_DIR should default to DATA_DIR")
	}
	for _, f := range []string{"persona.md", "profile.md"} {
		if _, err := os.Stat(filepath.Join(app.cfg.DataDir, f)); err != nil {
			t.Fatalf("%s should be seeded in the data dir: %v", f, err)
		}
	}
}
