// Package bot wires Telegram, the models and the memory together: one chat turn, commands,
// and background maintenance. Message content is never logged.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"companion/internal/assets"
	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/memory"
	"companion/internal/prompt"
	"companion/internal/store"
	"companion/internal/telegram"
)

const help = `可用命令：
/memory [关键词]  查看记忆（带关键词则按相关度检索）
/forget <id>  永久删除一条记忆
/edit <id> <新内容>  修改一条记忆
/profile  查看核心档案；/profile set <内容> 手动设置；/profile reload 从 profile.md 重新载入
/learning  查看学习进度
/pin  把我刚才的回复固定为“说话风格示例”（换模型后用来保持语气）
/pins  查看风格示例；/unpin <序号> 取消
/reembed  换了嵌入模型后，重新计算记忆向量
/consolidate  立即整理记忆（会先自动备份）
/status  查看后端、记忆与 token 用量（含缓存命中率）
/balance  查询 DeepSeek 账户余额
/visiontest  检测读图模型是否真的能读图
发送图片即可读图（公式、手写、截图）。`

const describePrompt = "请完整转写图片中的文字；数学公式用 LaTeX 写出，看不清或不确定的符号用 [?] 标出，不要猜。" +
	"转写之后，用一两句话客观描述图片内容。只输出转写与描述。"

// Messenger is the part of the Telegram client the bot needs (fakeable in tests).
type Messenger interface {
	Updates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error)
	Send(ctx context.Context, chat int64, text string) error
	Typing(ctx context.Context, chat int64)
	Download(ctx context.Context, fileID string) ([]byte, error)
}

type Deps struct {
	Store  *store.Store
	Chat   llm.Completer
	Maint  llm.Completer
	Vision llm.Completer // nil disables image reading
	Embed  llm.Embedder
	TG     Messenger
	Now    func() time.Time
}

type App struct {
	cfg           *config.Config
	st            *store.Store
	chat          llm.Completer
	vision        llm.Completer
	emb           llm.Embedder
	mm            *memory.Manager
	tg            Messenger
	now           func() time.Time
	personaPath   string
	profilePath   string
	personaWarned atomic.Bool
	tr            *tracker
	httpc         *http.Client
	fetchc        *http.Client  // web_fetch (SEARCH_PROVIDER=searxng): guarded against internal addresses
	visionState   atomic.Int32  // visionUnknown / visionOK / visionBad, see visionprobe.go
	probeCode     func() string // tests override the random probe code

	mu           sync.Mutex
	chatLocks    map[int64]*sync.Mutex
	lastActivity map[int64]time.Time
	turns        map[int64]int
	maintMu      sync.Mutex
	wg           sync.WaitGroup
	bg           context.Context
}

func New(cfg *config.Config, d Deps) (*App, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	a := &App{
		cfg: cfg, st: d.Store, chat: d.Chat, vision: d.Vision, emb: d.Embed, tg: d.TG, now: d.Now,
		personaPath:  filepath.Join(cfg.ConfigDir, "persona.md"),
		profilePath:  filepath.Join(cfg.ConfigDir, "profile.md"),
		chatLocks:    map[int64]*sync.Mutex{},
		lastActivity: map[int64]time.Time{},
		turns:        map[int64]int{},
		bg:           context.Background(),
		tr:           &tracker{st: d.Store},
		httpc:        &http.Client{},
	}
	a.fetchc = newFetchClient(cfg.FetchAllowPrivate, a.checkFetchHost)
	for kind, c := range map[string]llm.Completer{"chat": d.Chat, "maint": d.Maint, "vision": d.Vision} {
		if cl, ok := c.(*llm.Client); ok && cl != nil {
			cl.OnUsage = a.tr.hook(kind)
		}
	}
	maintModel := cfg.Maint.Model
	a.mm = &memory.Manager{St: d.Store, Emb: d.Embed, Maint: d.Maint, MaintModel: maintModel, Cfg: cfg, Now: d.Now}
	if err := a.seedFiles(); err != nil {
		return nil, err
	}
	return a, nil
}

// seedFiles creates the example persona.md / profile.md when they live in the data directory (the default).
// A separate CONFIG_DIR belongs to the user and is never written to: missing files simply mean built-in defaults.
func (a *App) seedFiles() error {
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return err
	}
	if a.cfg.ConfigDir == a.cfg.DataDir {
		if _, err := os.Stat(a.personaPath); os.IsNotExist(err) {
			if err := os.WriteFile(a.personaPath, []byte(assets.PersonaExample), 0o600); err != nil {
				return err
			}
		}
		if _, err := os.Stat(a.profilePath); os.IsNotExist(err) {
			if err := os.WriteFile(a.profilePath, []byte(assets.ProfileExample), 0o600); err != nil {
				return err
			}
		}
	}
	b, err := os.ReadFile(a.profilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cannot read profile.md; check its permissions (the container user needs read access, e.g. chmod 644)", "err", err)
		}
		return nil
	}
	// Seed the core profile only from a profile.md the user actually edited (not the placeholder).
	text := strings.TrimSpace(string(b))
	if a.st.Core() == "" && text != "" && text != strings.TrimSpace(assets.ProfileExample) {
		return a.st.SetCore(text, "seed from profile.md")
	}
	return nil
}

// loadPersona reads persona.md on every turn, so edits take effect on the next message. A missing or
// unreadable file falls back to the built-in example persona.
func (a *App) loadPersona() string {
	b, err := os.ReadFile(a.personaPath)
	if err == nil {
		return string(b)
	}
	if !os.IsNotExist(err) && a.personaWarned.CompareAndSwap(false, true) {
		slog.Warn("cannot read persona.md; using the built-in default persona (check file permissions, e.g. chmod 644)", "err", err)
	}
	return assets.PersonaExample
}

// ---------------------------------------------------------------- main loop

func (a *App) Run(ctx context.Context) error {
	a.bg = ctx
	offset := a.st.Offset()
	a.goBG(func() { a.maintenanceLoop(ctx) })
	if a.cfg.AutoReembed {
		a.goBG(func() {
			if n := a.mm.NeedReembed(); n > 0 {
				slog.Info("re-embedding memories for the current embedding model", "count", n)
				a.maintMu.Lock()
				defer a.maintMu.Unlock()
				if done, err := a.mm.Reembed(ctx); err != nil {
					slog.Warn("re-embed failed", "done", done, "err", err)
				}
			}
		})
	}
	if a.vision != nil && a.cfg.VisionProbe {
		a.goBG(func() { a.startupProbe(ctx) })
	}
	if a.cfg.APIAddr != "" {
		a.goBG(func() {
			if err := a.serveAPI(ctx); err != nil {
				slog.Error("API server stopped", "err", err)
			}
		})
	}
	if a.tg == nil {
		slog.Info("running API-only (no TELEGRAM_TOKEN)")
		<-ctx.Done()
		a.drain()
		return nil
	}
	slog.Info("bot started", "allowed_users", len(a.cfg.AllowedIDs))
	for ctx.Err() == nil {
		ups, err := a.tg.Updates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("polling error", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			offset = max(offset, u.UpdateID+1)
			u := u
			a.goBG(func() { a.safeHandle(ctx, u) })
		}
		if len(ups) > 0 {
			if err := a.st.SetOffset(offset); err != nil {
				slog.Warn("saving offset failed", "err", err)
			}
		}
		a.heartbeat()
	}
	a.drain()
	return nil
}

func (a *App) drain() {
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		slog.Warn("shutdown: background work still running after 20s")
	}
}

func (a *App) heartbeat() {
	_ = os.WriteFile(filepath.Join(a.cfg.DataDir, ".heartbeat"), []byte(fmt.Sprint(a.now().Unix())), 0o600)
}

func (a *App) goBG(f func()) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background task panicked", "panic", r)
			}
		}()
		f()
	}()
}

// Wait blocks until all in-flight background work finishes (used by tests).
func (a *App) Wait() { a.wg.Wait() }

func (a *App) safeHandle(ctx context.Context, u telegram.Update) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while handling update", "panic", r)
		}
	}()
	a.Handle(ctx, u)
}

func (a *App) chatLock(chat int64) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.chatLocks[chat]
	if l == nil {
		l = &sync.Mutex{}
		a.chatLocks[chat] = l
	}
	return l
}

func (a *App) touch(chat int64) {
	a.mu.Lock()
	a.lastActivity[chat] = a.now()
	a.mu.Unlock()
}

func (a *App) send(ctx context.Context, chat int64, text string) {
	if err := a.tg.Send(ctx, chat, text); err != nil {
		slog.Warn("send failed", "err", err)
	}
}

// Handle processes one Telegram update.
func (a *App) Handle(ctx context.Context, u telegram.Update) {
	m := u.Message
	if m == nil || m.Chat.Type != "private" {
		return
	}
	chat := m.Chat.ID
	var uid int64
	if m.From != nil {
		uid = m.From.ID
	}
	if len(a.cfg.AllowedIDs) == 0 {
		a.send(ctx, chat, fmt.Sprintf("尚未配置白名单。你的用户 ID 是 %d，请把它写入 ALLOWED_USER_IDS 后重启。", uid))
		return
	}
	if !a.cfg.AllowedIDs[uid] {
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = strings.TrimSpace(m.Caption)
	}
	fileID, mime := "", "image/jpeg"
	if len(m.Photo) > 0 {
		fileID = m.Photo[len(m.Photo)-1].FileID
	} else if m.Document != nil && strings.HasPrefix(m.Document.MimeType, "image/") {
		fileID, mime = m.Document.FileID, m.Document.MimeType
	}

	l := a.chatLock(chat)
	l.Lock()
	defer l.Unlock()
	a.touch(chat)
	defer a.touch(chat)
	switch {
	case strings.HasPrefix(text, "/") && fileID == "":
		a.command(ctx, chat, text)
	case text != "" || fileID != "":
		a.turn(ctx, chat, text, fileID, mime)
	}
}

// ---------------------------------------------------------------- one chat turn

// Image is an image attached to a request.
type Image struct {
	Data []byte
	Mime string
}

var (
	errVisionOff = errors.New("当前没有启用读图后端（设置 VISION_ENABLED=true，并确保模型支持图片输入）")
	errEmpty     = errors.New("模型没有返回内容，请再说一次")
)

func (a *App) turn(ctx context.Context, chat int64, text, fileID, mime string) {
	a.tg.Typing(ctx, chat)
	var imgs []Image
	if fileID != "" {
		if a.vision == nil {
			a.send(ctx, chat, errVisionOff.Error())
			return
		}
		data, err := a.tg.Download(ctx, fileID)
		if err != nil {
			a.send(ctx, chat, "下载图片失败："+truncate(err.Error(), 200))
			return
		}
		imgs = []Image{{Data: data, Mime: mime}}
	}
	reply, err := a.Respond(ctx, chat, text, imgs)
	if err != nil {
		slog.Warn("turn failed", "err", err)
		a.send(ctx, chat, "出错了："+truncate(err.Error(), 200))
		return
	}
	a.send(ctx, chat, reply)
}

// Respond runs one conversation turn for a chat (memory retrieval, prompt, model call, persona,
// persistence, background maintenance scheduling). Callers must hold chatLock(chat). Frontend-independent.
func (a *App) Respond(ctx context.Context, chat int64, text string, imgs []Image) (string, error) {
	r, err := a.RespondTools(ctx, chat, text, imgs, TurnOpts{})
	return r.Text, err
}

// TurnOpts carries the tool-calling state of an API request.
type TurnOpts struct {
	Tools json.RawMessage // tools declared by the client (OpenAI format)
	Tail  []llm.Message   // the client's messages after the user message: assistant tool_calls + tool results
}

// Reply is a final text answer, or (ToolCalls set) a request for the client to run tools and call again.
type Reply struct {
	Text      string
	ToolCalls json.RawMessage
	Reasoning string
}

func (a *App) describeImages(ctx context.Context, imgs []Image) (string, error) {
	if a.vision == nil {
		return "", errVisionOff
	}
	if a.visionState.Load() == visionBad {
		return "", errVisionBad
	}
	var parts []string
	for _, im := range imgs {
		desc, err := a.vision.Complete(ctx, []llm.Message{llm.Image(describePrompt, im.Data, im.Mime)}, 0.1)
		if err != nil {
			return "", fmt.Errorf("读图失败：%w", err)
		}
		parts = append(parts, "[用户发送了一张图片。图片内容转写：\n"+desc+"\n]")
	}
	return strings.Join(parts, "\n"), nil
}

func countRounds(tail []llm.Message) int {
	n := 0
	for _, m := range tail {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			n++
		}
	}
	return n
}

const finishNow = "工具调用次数已达上限，请不要再调用任何工具，直接根据已有信息回答。"

// RespondTools is Respond with tool calling. When opts.Tail is non-empty and the chat's last stored message is
// still the user message of this turn, the turn continues (the user message is not stored again).
func (a *App) RespondTools(ctx context.Context, chat int64, text string, imgs []Image, opts TurnOpts) (Reply, error) {
	var composed string
	var userID int64
	cont := false
	if len(opts.Tail) > 0 {
		if u, ok := a.st.LastByRole(chat, "user", 0); ok {
			if as, hasA := a.st.LastByRole(chat, "assistant", 0); !hasA || as.ID < u.ID {
				composed, userID, cont = u.Content, u.ID, true
			}
		}
	}
	if !cont {
		composed = text
		if len(imgs) > 0 {
			desc, err := a.describeImages(ctx, imgs)
			if err != nil {
				return Reply{}, err
			}
			composed = strings.TrimSpace(desc + "\n" + text)
		}
		if composed == "" {
			return Reply{}, errors.New("empty message")
		}
		id, err := a.st.AddMessage(chat, "user", composed)
		if err != nil {
			return Reply{}, fmt.Errorf("无法保存消息：%w", err)
		}
		userID = id
	}
	query := composed
	if len([]rune(composed)) < 12 {
		if prev, ok := a.st.LastByRole(chat, "user", userID); ok {
			query = prev.Content + "\n" + composed
		}
	}
	mems, err := a.mm.Retrieve(ctx, query, 0)
	if err != nil { // the embedding service being down must not stop the conversation
		slog.Warn("memory retrieval failed; continuing without memories", "err", err)
		mems = nil
	}
	persona := a.loadPersona()
	var recent []store.Message
	for _, m := range a.st.Unsummarized(chat) {
		if m.ID != userID {
			recent = append(recent, m)
		}
	}
	// Budget 1.5x: the window is trimmed in batches by RollSummary (at 1.25x, down to 0.6x) instead of by one
	// message per turn, which would change the prompt prefix every turn and defeat the provider's prefix cache.
	msgs := prompt.Build(prompt.Input{
		Persona: persona, Core: a.st.Core(), Summary: a.st.Summary(chat), Pins: a.st.Pins(),
		Recent: recent, RecentBudget: a.cfg.RecentTokens * 3 / 2, Memories: mems, Learning: a.st.ListLearning(8),
		Now: a.now().In(a.cfg.Loc), UserText: composed,
	})
	msgs = append(msgs, opts.Tail...)

	tools, err := a.mergeTools(opts.Tools)
	if err != nil {
		return Reply{}, err
	}
	tc, canTools := a.chat.(llm.ToolCompleter)
	if len(tools) > 0 && !canTools {
		if len(opts.Tools) > 0 {
			return Reply{}, errors.New("the chat backend does not support tools")
		}
		tools = nil
	}
	rounds := countRounds(opts.Tail)
	var res llm.Result
	for {
		limited := rounds >= a.cfg.MaxToolRounds
		if limited {
			msgs = append(msgs, llm.Text("system", finishNow))
		}
		if len(tools) > 0 {
			res, err = tc.CompleteTools(ctx, msgs, tools, a.cfg.Temperature)
		} else {
			res.Text, err = a.chat.Complete(ctx, msgs, a.cfg.Temperature)
			res.ToolCalls = nil
		}
		if err != nil {
			return Reply{}, err
		}
		if len(res.ToolCalls) == 0 {
			break
		}
		if limited {
			return Reply{}, errors.New("model kept requesting tools after the limit")
		}
		var calls []toolCall
		if err := json.Unmarshal(res.ToolCalls, &calls); err != nil || len(calls) == 0 {
			return Reply{}, errors.New("malformed tool_calls from model")
		}
		allBuiltin := true
		for _, c := range calls {
			if !a.isBuiltin(c.Function.Name) {
				allBuiltin = false
			}
		}
		if !allBuiltin { // the client runs these; it will call again with the results
			return Reply{Text: res.Text, ToolCalls: res.ToolCalls, Reasoning: res.Reasoning}, nil
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: res.Text, ToolCalls: res.ToolCalls, ReasoningContent: res.Reasoning})
		for _, c := range calls {
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: c.ID, Content: a.runBuiltin(ctx, c)})
		}
		rounds++
	}
	reply := res.Text
	if reply == "" {
		return Reply{}, errEmpty
	}
	if _, err := a.st.AddMessage(chat, "assistant", reply); err != nil {
		slog.Warn("saving reply failed", "err", err)
	}
	a.mm.Touch(mems)

	a.mu.Lock()
	a.turns[chat]++
	due := a.turns[chat] >= a.cfg.ExtractEvery
	if due {
		a.turns[chat] = 0
	}
	a.mu.Unlock()
	if due {
		a.goBG(func() {
			if _, err := a.runMaintenance(a.bg, chat, false); err != nil {
				slog.Warn("maintenance failed", "err", err)
			}
		})
	}
	return Reply{Text: reply}, nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---------------------------------------------------------------- background maintenance

func (a *App) runMaintenance(ctx context.Context, chat int64, consolidate bool) (*memory.Stats, error) {
	a.maintMu.Lock()
	defer a.maintMu.Unlock()
	if _, err := a.mm.Extract(ctx, chat); err != nil {
		return nil, err
	}
	if _, err := a.mm.RollSummary(ctx, chat); err != nil {
		return nil, err
	}
	if consolidate {
		s, err := a.mm.Consolidate(ctx)
		return &s, err
	}
	return nil, nil
}

func (a *App) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.heartbeat()
			a.MaintenanceTick(ctx)
		}
	}
}

// MaintenanceTick extracts memories for idle chats and consolidates when due.
func (a *App) MaintenanceTick(ctx context.Context) {
	now := a.now()
	last := time.Unix(int64(a.st.MetaFloat("last_consolidation")), 0)
	due := now.Sub(last) > time.Duration(a.cfg.ConsolidateHours)*time.Hour
	for _, chat := range a.st.ChatIDs() {
		a.mu.Lock()
		act := a.lastActivity[chat]
		a.mu.Unlock()
		lastMsg := time.Unix(int64(a.st.LastMessageTS(chat)), 0)
		if lastMsg.After(act) {
			act = lastMsg
		}
		if now.Sub(act) < time.Duration(a.cfg.IdleMinutes)*time.Minute {
			continue
		}
		pending := len(a.st.MessagesAfter(chat, a.st.ExtractedUpTo(chat), 1)) > 0
		if !pending && !due {
			continue
		}
		if _, err := a.runMaintenance(ctx, chat, due); err != nil {
			slog.Warn("idle maintenance failed", "err", err)
			continue
		}
		due = false
	}
}

// ---------------------------------------------------------------- commands

func (a *App) command(ctx context.Context, chat int64, text string) {
	head, rest, _ := strings.Cut(text, " ")
	cmd, _, _ := strings.Cut(head, "@")
	cmd = strings.ToLower(cmd)
	rest = strings.TrimSpace(rest)
	reply := func(s string) { a.send(ctx, chat, s) }

	switch cmd {
	case "/start", "/help":
		reply(help)
	case "/memory":
		var mems []store.Memory
		if rest != "" {
			var err error
			if mems, err = a.mm.Retrieve(ctx, rest, 15); err != nil {
				reply("检索失败：" + truncate(err.Error(), 200))
				return
			}
		} else {
			mems = a.st.ActiveMemories()
			sort.Slice(mems, func(i, j int) bool { return mems[i].ID > mems[j].ID })
			if len(mems) > 15 {
				mems = mems[:15]
			}
		}
		if len(mems) == 0 {
			reply("没有找到记忆。")
			return
		}
		var b strings.Builder
		for _, m := range mems {
			fmt.Fprintf(&b, "#%d [%s·%d] %s\n", m.ID, m.Kind, m.Importance, m.Text)
		}
		reply(strings.TrimSpace(b.String()))
	case "/forget":
		var id int64
		if _, err := fmt.Sscanf(rest, "%d", &id); err != nil {
			reply("用法：/forget <记忆id>")
			return
		}
		ok, err := a.st.DeleteMemory(id)
		switch {
		case err != nil:
			reply("删除失败：" + truncate(err.Error(), 200))
		case ok:
			reply(fmt.Sprintf("已永久删除记忆 #%d。（原始聊天日志不受影响，见 README。）", id))
		default:
			reply("没有这条记忆。")
		}
	case "/edit":
		idStr, newText, _ := strings.Cut(rest, " ")
		newText = strings.TrimSpace(newText)
		var id int64
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || newText == "" {
			reply("用法：/edit <记忆id> <新内容>")
			return
		}
		if _, ok := a.st.GetMemory(id); !ok {
			reply("没有这条记忆。")
			return
		}
		v, err := a.emb.Embed(ctx, []string{newText})
		if err != nil {
			reply("嵌入服务不可用：" + truncate(err.Error(), 200))
			return
		}
		if err := a.st.EditMemory(id, newText, a.emb.Model(), v[0]); err != nil {
			reply("更新失败：" + truncate(err.Error(), 200))
			return
		}
		reply(fmt.Sprintf("已更新记忆 #%d。", id))
	case "/profile":
		if strings.TrimSpace(rest) == "reload" {
			b, err := os.ReadFile(a.profilePath)
			if err != nil {
				reply("读取 profile.md 失败：" + truncate(err.Error(), 200))
				return
			}
			text := strings.TrimSpace(string(b))
			if text == "" || text == strings.TrimSpace(assets.ProfileExample) {
				reply("profile.md 还是空白模板，请先填好再 /profile reload。")
				return
			}
			if err := a.st.SetCore(text, "reload profile.md"); err != nil {
				reply("保存失败：" + truncate(err.Error(), 200))
				return
			}
			reply("核心档案已从 profile.md 重新载入。")
			return
		}
		if after, ok := strings.CutPrefix(rest, "set "); ok {
			if err := a.st.SetCore(strings.TrimSpace(after), "manual"); err != nil {
				reply("保存失败：" + truncate(err.Error(), 200))
				return
			}
			reply("核心档案已更新。")
			return
		}
		if c := a.st.Core(); c != "" {
			reply(c)
		} else {
			reply("（核心档案为空，可用 /profile set <内容> 设置）")
		}
	case "/learning":
		ls := a.st.ListLearning(20)
		if len(ls) == 0 {
			reply("还没有学习进度记录。")
			return
		}
		var b strings.Builder
		for _, l := range ls {
			fmt.Fprintf(&b, "%s [%s]", l.Topic, l.Status)
			if l.ErrorPattern != "" {
				b.WriteString(" — " + l.ErrorPattern)
			}
			b.WriteByte('\n')
		}
		reply(strings.TrimSpace(b.String()))
	case "/pin":
		last, ok := a.st.LastByRole(chat, "assistant", 0)
		if !ok {
			reply("还没有可以固定的回复。")
			return
		}
		if err := a.st.AddPin(truncate(last.Content, 600), a.cfg.MaxPins); err != nil {
			reply("保存失败：" + truncate(err.Error(), 200))
			return
		}
		reply(fmt.Sprintf("已固定为风格示例（最多保留 %d 条，超出会挤掉最早的）。", a.cfg.MaxPins))
	case "/pins":
		pins := a.st.Pins()
		if len(pins) == 0 {
			reply("还没有风格示例。回复你喜欢的语气后用 /pin 固定。")
			return
		}
		var b strings.Builder
		for i, p := range pins {
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(p, 120))
		}
		reply(strings.TrimSpace(b.String()))
	case "/unpin":
		var n int
		if _, err := fmt.Sscanf(rest, "%d", &n); err != nil || !a.st.RemovePin(n) {
			reply("用法：/unpin <序号>（序号见 /pins）")
			return
		}
		reply("已取消。")
	case "/reembed":
		n := a.mm.NeedReembed()
		if n == 0 {
			reply("所有记忆的向量都已匹配当前嵌入模型。")
			return
		}
		reply(fmt.Sprintf("重新计算 %d 条记忆的向量…", n))
		a.maintMu.Lock()
		done, err := a.mm.Reembed(ctx)
		a.maintMu.Unlock()
		if err != nil {
			reply(fmt.Sprintf("已完成 %d 条，出错：%s", done, truncate(err.Error(), 200)))
			return
		}
		reply(fmt.Sprintf("完成：%d 条。", done))
	case "/consolidate":
		reply("开始整理记忆（已先备份）…")
		stats, err := a.runMaintenance(ctx, chat, true)
		if err != nil {
			reply("整理没有完成：" + truncate(err.Error(), 200))
			return
		}
		reply(fmt.Sprintf("完成：归档 %d，合并 %d，核心档案%s。", stats.Archived, stats.Merged,
			map[bool]string{true: "已更新", false: "未变化"}[stats.CoreUpdated]))
	case "/status":
		c := a.st.CountMemories()
		last := "从未"
		if f := a.st.MetaFloat("last_consolidation"); f > 0 {
			last = time.Unix(int64(f), 0).In(a.cfg.Loc).Format("01-02 15:04")
		}
		vision := "关"
		if a.vision != nil {
			vision = "开"
		}
		out := fmt.Sprintf("对话模型：%s\n整理模型：%s\n读图：%s\n联网搜索：%s\n嵌入模型：%s\n记忆：有效 %d / 已取代 %d / 归档 %d（待重新嵌入 %d）\n风格示例：%d 条\n上次整理：%s",
			a.cfg.LLM.Model, a.cfg.Maint.Model, vision, map[bool]string{true: "开", false: "关"}[a.cfg.SearchEnabled],
			a.emb.Model(), c["active"], c["superseded"], c["archived"],
			a.mm.NeedReembed(), len(a.st.Pins()), last)
		for _, k := range [][2]string{{"chat", "对话"}, {"maint", "后台整理"}, {"vision", "读图"}} {
			if l := a.describeUsage(k[0], k[1]); l != "" {
				out += "\n累计 " + l
			}
		}
		reply(out)
	case "/visiontest":
		if a.vision == nil {
			reply(errVisionOff.Error())
			return
		}
		if err := a.probeVision(ctx); err != nil {
			reply("读图自检未通过：" + truncate(err.Error(), 300))
			return
		}
		reply("读图自检通过：模型能读出测试图里的数字。")
	case "/balance":
		b, ok := a.chat.(interface {
			Balance(context.Context) (string, error)
		})
		if !ok {
			reply("当前后端不支持查询余额。")
			return
		}
		text, err := b.Balance(ctx)
		if err != nil {
			reply("查询失败：" + truncate(err.Error(), 200))
			return
		}
		reply("余额：" + text)
	default:
		reply("未知命令。发送 /help 查看可用命令。")
	}
}
