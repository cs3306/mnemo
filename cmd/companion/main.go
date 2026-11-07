// Command companion runs the Telegram companion bot and a few maintenance subcommands.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // embed the zone database so TZ works in scratch/distroless images

	"companion/internal/bot"
	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/memory"
	"companion/internal/store"
	"companion/internal/telegram"
)

var version = "dev"

const usage = `usage: companion [command]

  run           run the bot (default)
  healthcheck   exit 0 if the bot polled Telegram recently (for container health checks)
  export [file] write a model-independent JSON snapshot of memories (no vectors) to file or stdout
  import <file> import such a snapshot (stop the bot first); vectors are recomputed
  version`

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "run":
		os.Exit(run())
	case "healthcheck":
		os.Exit(healthcheck())
	case "export":
		os.Exit(export(os.Args[2:]))
	case "import":
		os.Exit(importSnapshot(os.Args[2:]))
	case "version", "--version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	return 1
}

func dataDir() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return d
	}
	return "/data"
}

func run() int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fail("%v", err)
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		return fail("configuration error:\n  - %s", joinLines(errs))
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fail("cannot open data directory: %v", err)
	}
	defer st.Close()

	chat := llm.New(cfg.LLM)
	deps := bot.Deps{
		Store: st,
		Chat:  chat,
		Maint: llm.New(cfg.Maint),
		Embed: llm.New(cfg.Embed),
	}
	if cfg.TelegramToken != "" { // keep deps.TG a true nil interface otherwise (API-only mode)
		deps.TG = telegram.New(cfg.TelegramToken, cfg.TGAPIBase)
	}
	if cfg.Vision != nil {
		deps.Vision = llm.New(*cfg.Vision)
	}
	app, err := bot.New(cfg, deps)
	if err != nil {
		return fail("%v", err)
	}
	slog.Info("starting", "version", version, "chat_model", cfg.LLM.Model, "embed_model", cfg.Embed.Model)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		return fail("%v", err)
	}
	return 0
}

func joinLines(errs []string) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "\n  - "
		}
		out += e
	}
	return out
}

func healthcheck() int {
	fi, err := os.Stat(filepath.Join(dataDir(), ".heartbeat"))
	if err != nil || time.Since(fi.ModTime()) > 180*time.Second {
		return 1
	}
	return 0
}

func export(args []string) int {
	st, err := store.OpenReadOnly(dataDir())
	if err != nil {
		return fail("cannot open data directory: %v", err)
	}
	b, err := json.MarshalIndent(st.Export(), "", "  ")
	if err != nil {
		return fail("%v", err)
	}
	if len(args) > 0 {
		if err := os.WriteFile(args[0], append(b, '\n'), 0o600); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	fmt.Println(string(b))
	return 0
}

func importSnapshot(args []string) int {
	if len(args) != 1 {
		return fail("usage: companion import <file>")
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return fail("%v", err)
	}
	var ex store.Export
	if err := json.Unmarshal(b, &ex); err != nil {
		return fail("not a valid export file: %v", err)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fail("%v", err)
	}
	if cfg.Embed.BaseURL == "" || cfg.Embed.Model == "" {
		return fail("EMBED_BASE_URL and EMBED_MODEL are required to recompute vectors")
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fail("cannot open data directory: %v", err)
	}
	defer st.Close()
	n, err := st.Import(ex)
	if err != nil {
		return fail("import failed: %v", err)
	}
	mm := &memory.Manager{St: st, Emb: llm.New(cfg.Embed), Cfg: cfg}
	done, err := mm.Reembed(context.Background())
	if err != nil {
		return fail("imported %d memories but re-embedding failed after %d: %v (run /reembed later)", n, done, err)
	}
	fmt.Printf("imported %d memories, computed %d vectors\n", n, done)
	return 0
}
