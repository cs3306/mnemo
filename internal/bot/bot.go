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
	"regexp"
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
	"companion/internal/speech"
	"companion/internal/store"
	"companion/internal/style"
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
/reset  让对话从头开始（清空摘要与近期窗口，保留记忆/档案/风格示例）
/style  风格笔记与复盘：/style review 立即复盘；approve / reject 审批提案；rollback 回滚；bad <原因> 标记刚才那句说得不好
/status  查看后端、记忆与 token 用量（含缓存命中率）
/balance  查询 DeepSeek 账户余额
/visiontest  检测读图模型是否真的能读图
/voice  语音回复：/voice on 总是语音；auto 仅当我发语音时；weighted 按概率随机；off 关闭（你说“发条语音”时一定发语音）
/plugins  查看插件工具；/plugins reload 重新发现
发送图片即可读图（公式、手写、截图）；发送语音消息会先转成文字再回答（需启用 STT）。`

const describePrompt = "请完整转写图片中的文字；数学公式用 LaTeX 写出，看不清或不确定的符号用 [?] 标出，不要猜。" +
	"转写之后，用一两句话客观描述图片内容。只输出转写与描述。"

// commandMenu is what Telegram shows when the user types "/" or taps the menu button. Commands that change
// a lot at once (/consolidate, /reembed) or need an argument to be useful (/unpin) stay out of it so that a
// stray tap does nothing heavy; /help lists everything. Every entry must also appear in help (tested).
var commandMenu = []telegram.BotCommand{
	{Command: "help", Description: "查看所有命令"},
	{Command: "memory", Description: "查看记忆（后面加关键词可检索）"},
	{Command: "forget", Description: "删除一条记忆：/forget <id>"},
	{Command: "edit", Description: "修改一条记忆：/edit <id> <新内容>"},
	{Command: "profile", Description: "查看核心档案"},
	{Command: "learning", Description: "查看学习进度"},
	{Command: "pin", Description: "把我刚才的回复固定为说话风格示例"},
	{Command: "pins", Description: "查看风格示例"},
	{Command: "style", Description: "风格笔记、复盘与审批"},
	{Command: "status", Description: "后端、记忆与 token 用量"},
	{Command: "balance", Description: "DeepSeek 账户余额"},
	{Command: "plugins", Description: "查看插件工具"},
	{Command: "visiontest", Description: "检测读图模型是否真的能读图"},
	{Command: "voice", Description: "语音回复：on / auto / weighted / off"},
}

// CommandSetter is implemented by the real Telegram client; test fakes may skip it.
type CommandSetter interface {
	SetCommands(ctx context.Context, chat int64, cmds []telegram.BotCommand) error
}

func (a *App) registerCommands(ctx context.Context) {
	cs, ok := a.tg.(CommandSetter)
	if !ok {
		return
	}
	for id := range a.cfg.AllowedIDs {
		if err := cs.SetCommands(ctx, id, commandMenu); err != nil {
			slog.Warn("cannot publish the command menu", "chat", id, "err", err)
		}
	}
}

// Messenger is the part of the Telegram client the bot needs (fakeable in tests).
type Messenger interface {
	Updates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error)
	Send(ctx context.Context, chat int64, text string) error
	Typing(ctx context.Context, chat int64)
	Download(ctx context.Context, fileID string) ([]byte, error)
}

// Transcriber turns a voice note into text; Synthesizer turns a reply into audio (internal/speech implements both).
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, name, language string) (string, error)
}
type Synthesizer interface {
	Synthesize(ctx context.Context, text, voice, format string) ([]byte, error)
}

// VoiceSender is implemented by the real Telegram client; test fakes may skip it.
type VoiceSender interface {
	SendVoice(ctx context.Context, chat int64, name string, data []byte) error
}

type Deps struct {
	Store  *store.Store
	Chat   llm.Completer
	Maint  llm.Completer
	Vision llm.Completer // nil disables image reading
	Review llm.Completer // style review; nil uses Maint
	STT    Transcriber   // nil: voice messages are not understood
	TTS    Synthesizer   // nil: no voice replies
	Embed  llm.Embedder
	TG     Messenger
	Now    func() time.Time
}

type App struct {
	cfg           *config.Config
	st            *store.Store
	chat          llm.Completer
	vision        llm.Completer
	stt           Transcriber
	tts           Synthesizer
	ttsMode       atomic.Int32   // ttsOff / ttsAuto / ttsAlways, changed by /voice
	rnd           func() float64 // [0,1); tests override it (weighted voice mode)
	vmu           sync.Mutex
	vrun          map[int64]int // per chat: voice replies in a row (>0) or typed replies in a row (<0), see voicepick.go
	review        llm.Completer
	style         *style.Store
	reviewing     atomic.Bool
	emb           llm.Embedder
	mm            *memory.Manager
	tg            Messenger
	now           func() time.Time
	personaPath   string
	profilePath   string
	personaWarned atomic.Bool
	tr            *tracker
	httpc         *http.Client
	evMu          sync.Mutex
	evQ           map[string]*eventQueue // per plugin server: events waiting to be delivered, in order
	fetchc        *http.Client           // web_fetch (SEARCH_PROVIDER=searxng): guarded against internal addresses
	plug          *pluginSet             // tool plugin servers (PLUGIN_URLS), see plugins.go
	visionState   atomic.Int32           // visionUnknown / visionOK / visionBad, see visionprobe.go
	probeCode     func() string          // tests override the random probe code

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
	a.stt, a.tts = d.STT, d.TTS
	switch cfg.TTSMode {
	case "off":
		a.ttsMode.Store(ttsOff)
	case "always":
		a.ttsMode.Store(ttsAlways)
	case "weighted":
		a.ttsMode.Store(ttsWeighted)
	default:
		a.ttsMode.Store(ttsAuto)
	}
	a.review = d.Review
	if a.review == nil {
		a.review = d.Maint
	}
	sty, err := style.Open(filepath.Join(cfg.DataDir, "style.json"), d.Now)
	if err != nil {
		return nil, fmt.Errorf("cannot open style notes: %w", err)
	}
	a.style = sty
	a.plug = newPluginSet(cfg.PluginURLs)
	a.fetchc = newFetchClient(cfg.FetchAllowPrivate, a.checkFetchHost)
	for kind, c := range map[string]llm.Completer{"chat": d.Chat, "maint": d.Maint, "vision": d.Vision, "review": d.Review} {
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
	if len(a.cfg.PluginURLs) > 0 {
		a.goBG(func() {
			slog.Info("plugins", "status", a.discoverAll(ctx))
			a.emit("startup", a.defaultChat(), nil)
		})
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
	a.goBG(func() { a.registerCommands(ctx) })
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

// RichSender is implemented by the real Telegram client; test fakes may skip it.
type RichSender interface {
	SendRich(ctx context.Context, chat int64, text string) error
}

// sendReply delivers model-written text. With TELEGRAM_RICH on and a client that supports it, the text goes out as a
// rich message (tables, LaTeX); anything else — status lines, errors, receipts — keeps using plain send.
func (a *App) sendReply(ctx context.Context, chat int64, text string) {
	if a.cfg.TGRich {
		if rs, ok := a.tg.(RichSender); ok {
			if err := rs.SendRich(ctx, chat, text); err != nil {
				slog.Warn("rich send failed", "err", err)
			}
			return
		}
	}
	a.send(ctx, chat, text)
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
	var doc *telegram.Document // a non-image file: stored by a plugin, not shown to the model
	if len(m.Photo) > 0 {
		fileID = m.Photo[len(m.Photo)-1].FileID
	} else if m.Document != nil && strings.HasPrefix(m.Document.MimeType, "image/") {
		fileID, mime = m.Document.FileID, m.Document.MimeType
	} else if m.Document != nil {
		doc = m.Document
	}

	var voice *telegram.Voice // a voice note (or audio file): transcribed first, then handled like typed text
	if m.Voice != nil {
		voice = m.Voice
	} else if m.Audio != nil && strings.HasPrefix(m.Audio.MimeType, "audio/") {
		voice = m.Audio
	}

	if text != "" || fileID != "" || doc != nil || voice != nil {
		kind := "text"
		switch {
		case strings.HasPrefix(text, "/") && fileID == "" && doc == nil && voice == nil:
			kind = "command"
		case voice != nil:
			kind = "voice"
		case fileID != "":
			kind = "image"
		case doc != nil:
			kind = "file"
		}
		a.emit("user_message", chat, map[string]any{"kind": kind}) // before the chat lock: a long running turn must not delay it
	}
	l := a.chatLock(chat)
	l.Lock()
	defer l.Unlock()
	a.touch(chat)
	defer a.touch(chat)
	switch {
	case strings.HasPrefix(text, "/") && fileID == "" && doc == nil && voice == nil:
		a.command(ctx, chat, text)
	case text != "" || fileID != "" || doc != nil || voice != nil:
		if voice != nil {
			var ok bool
			if text, ok = a.receiveVoice(ctx, chat, voice, text); !ok {
				return
			}
		}
		if doc != nil {
			var ok bool
			if text, ok = a.receiveDoc(ctx, chat, doc, text); !ok {
				return
			}
		}
		a.turn(ctx, chat, text, fileID, mime, voice != nil)
	}
}

// receiveVoice downloads and transcribes a voice note, shows the user what was heard (so a misheard word is
// visible) and returns the text to answer. A caption is kept in front of the transcript.
func (a *App) receiveVoice(ctx context.Context, chat int64, v *telegram.Voice, caption string) (string, bool) {
	if a.stt == nil {
		a.send(ctx, chat, "当前没有启用语音识别（设置 STT_ENABLED=true 以及 STT_BASE_URL / STT_MODEL）")
		return "", false
	}
	a.tg.Typing(ctx, chat)
	data, err := a.tg.Download(ctx, v.FileID)
	if err != nil {
		a.send(ctx, chat, "下载语音失败："+truncate(err.Error(), 200))
		return "", false
	}
	heard, err := a.stt.Transcribe(ctx, data, "voice"+audioExt(v.MimeType), a.cfg.STTLanguage)
	if err != nil {
		slog.Warn("transcription failed", "err", err)
		a.send(ctx, chat, "语音识别失败："+truncate(err.Error(), 200))
		return "", false
	}
	if heard == "" {
		a.send(ctx, chat, "没听清，请再说一次")
		return "", false
	}
	a.send(ctx, chat, "🎤 "+heard)
	if caption != "" {
		heard = caption + "\n" + heard
	}
	return heard, true
}

func audioExt(mime string) string {
	switch strings.ToLower(mime) {
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a", "audio/m4a", "audio/aac":
		return ".m4a"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/flac":
		return ".flac"
	default:
		return ".ogg" // Telegram voice notes are OGG/Opus
	}
}

const (
	ttsOff int32 = iota
	ttsAuto
	ttsAlways
	ttsWeighted // spoken with a probability, see voicepick.go
)

// wantVoice reports whether this turn's reply should also (or only) be spoken.
func (a *App) wantVoice(voiceIn bool, c voiceCue) bool {
	if a.tts == nil {
		return false
	}
	if _, ok := a.tg.(VoiceSender); !ok {
		return false
	}
	switch a.ttsMode.Load() {
	case ttsAlways:
		return true
	case ttsAuto:
		return voiceIn
	case ttsWeighted:
		w := a.cfg.TTSWeightText
		if voiceIn {
			w = a.cfg.TTSWeightVoice
		}
		c.Adapt = a.cfg.TTSAdapt
		if c.Adapt && len([]rune(c.Reply)) > a.cfg.TTSMaxChars {
			return false // too long to read out; speak() would refuse it anyway
		}
		return a.roll() < voiceProb(w, c, 2*a.roll()-1) // each reply rolls on its own
	}
	return false
}

// speak sends text as a voice message and reports whether it did. It never fails the turn: on any problem the
// caller still has the text reply.
func (a *App) speak(ctx context.Context, chat int64, text, voice string) bool {
	clean := speech.CleanForSpeech(text, a.cfg.TTSMaxChars)
	if clean == "" {
		return false
	}
	vs, ok := a.tg.(VoiceSender)
	if !ok || a.tts == nil {
		return false
	}
	a.tg.Typing(ctx, chat)
	format := a.cfg.TTSFormat
	if format == "" {
		format = "mp3"
	}
	if voice == "" {
		voice = a.cfg.TTSVoice
	}
	audio, err := a.tts.Synthesize(ctx, clean, voice, format)
	if err != nil {
		slog.Warn("speech synthesis failed", "err", err)
		return false
	}
	ext := ".mp3"
	if format == "opus" {
		ext = ".ogg"
	}
	if err := vs.SendVoice(ctx, chat, "reply"+ext, audio); err != nil {
		slog.Warn("sending voice failed", "err", err)
		return false
	}
	return true
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

func (a *App) turn(ctx context.Context, chat int64, text, fileID, mime string, voiceIn bool) {
	replied := true // every path says something to the user (an error notice counts), except an empty model reply
	defer func() { a.emit("turn_done", chat, map[string]any{"origin": "user", "replied": replied}) }()
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
	r, err := a.RespondTools(ctx, chat, text, imgs, TurnOpts{Deliver: a.canSendFiles(), Voice: a.voiceReady()})
	if err != nil {
		slog.Warn("turn failed", "err", err)
		a.send(ctx, chat, "出错了："+truncate(err.Error(), 200))
		return
	}
	replied = r.Text != "" || len(r.Files) > 0 || r.VoiceSent
	// What the user asked for wins ("发条语音" always speaks, "别发语音" never does); otherwise the mode decides.
	// She already spoke on her own (send_voice): do not read the text out again.
	ask := voiceAsk(text)
	voiceOut := false
	switch {
	case r.VoiceSent || ask < 0 || r.Text == "":
	case ask > 0:
		voiceOut = a.voiceReady()
	default:
		voiceOut = a.wantVoice(voiceIn, voiceCue{User: text, Reply: r.Text, Streak: a.streak(chat), Lean: r.VoiceLean})
	}
	spoke, voiced := false, r.VoiceSent
	if voiceOut {
		line := r.Text
		if ask > 0 && len([]rune(speech.CleanForSpeech(line, 0))) > a.cfg.TTSMaxChars {
			line = leadForSpeech(speech.CleanForSpeech(line, 0), a.cfg.TTSMaxChars) // requested but long: read the beginning
			a.sendReply(ctx, chat, r.Text)
			voiced = a.speak(ctx, chat, line, r.VoiceName)
			a.notePlay(chat, voiced)
			a.deliverFiles(ctx, chat, r.Files)
			return
		}
		if !a.cfg.TTSText {
			spoke = a.speak(ctx, chat, line, r.VoiceName) // voice only; fall back to text when speaking is not possible
			voiced = spoke
		}
	}
	if !spoke && r.Text != "" {
		a.sendReply(ctx, chat, r.Text)
		if voiceOut && a.cfg.TTSText {
			voiced = a.speak(ctx, chat, r.Text, r.VoiceName)
		}
	}
	a.notePlay(chat, voiced)
	a.deliverFiles(ctx, chat, r.Files)
}

// deliverFiles uploads the files a tool produced during the turn.
func (a *App) deliverFiles(ctx context.Context, chat int64, files []OutFile) {
	fs, ok := a.tg.(FileSender)
	if !ok {
		return
	}
	for _, f := range files {
		a.tg.Typing(ctx, chat)
		if err := fs.SendFile(ctx, chat, f.Name, f.Mime, f.Data); err != nil {
			slog.Warn("sending file failed", "name", f.Name, "err", err)
			a.send(ctx, chat, "文件发送失败（"+f.Name+"）："+truncate(err.Error(), 200))
		}
	}
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

	Deliver bool // the frontend can send files produced by plugin tools (Telegram)
	Voice   bool // the frontend can send voice messages, so the send_voice tool is offered (Telegram)

	// Instruction makes this a scheduled turn: nobody just spoke. The text is shown to the model as the latest
	// message but is never stored as something the user said; only the reply joins the conversation.
	Instruction string
}

// Reply is a final text answer, or (ToolCalls set) a request for the client to run tools and call again.
type Reply struct {
	Text      string
	ToolCalls json.RawMessage
	Reasoning string
	Files     []OutFile // produced by plugin tools during the turn, to be delivered by the frontend
	VoiceSent bool      // she already sent a voice message herself (send_voice) this turn
	VoiceLean int       // +1 / -1: she asked for voice / text with a leading [[语音]] / [[文字]] (a nudge for TTS_MODE=weighted)
	VoiceName string    // the voice she picked with a leading [[声:name]] (empty = default)
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

// finishNow is sent when the cap is reached: no tools are offered, so the model can only report. The report is stored like any
// reply, so it is what the next turn ("继续") starts from; tool results themselves are not carried over.
const finishNow = "本轮工具调用次数已用完，现在不能再调用任何工具。请直接汇报：已经完成了什么（含关键文件路径或结果）、目前卡在哪或做到哪一步、" +
	"下一步打算做什么。不要假装已经做完。用户回复“继续”后，你会在新的一轮里接着做。"

// graceNotice is sent once when the turn enters its last few rounds, so the model can wrap up instead of being cut off.
func graceNotice(left int) string {
	return fmt.Sprintf("提示：本轮最多还能再调用 %d 次工具。请把剩下的步骤合并成尽量少的调用；如果做不完，就在最后留出一次回复，"+
		"汇报已完成的部分、当前状态和下一步，用户说“继续”后再接着做。", left)
}

// RespondTools is Respond with tool calling. When opts.Tail is non-empty and the chat's last stored message is
// still the user message of this turn, the turn continues (the user message is not stored again).
func (a *App) RespondTools(ctx context.Context, chat int64, text string, imgs []Image, opts TurnOpts) (Reply, error) {
	var composed string
	var userID int64
	cont := false
	if len(opts.Tail) > 0 && opts.Instruction == "" {
		if u, ok := a.st.LastByRole(chat, "user", 0); ok {
			if as, hasA := a.st.LastByRole(chat, "assistant", 0); !hasA || as.ID < u.ID {
				composed, userID, cont = u.Content, u.ID, true
			}
		}
	}
	if opts.Instruction != "" {
		composed = opts.Instruction
	} else if !cont {
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
		Persona: persona, Style: a.style.Text(), Core: a.st.Core(), Summary: a.st.Summary(chat), Pins: a.st.Pins(),
		Recent: recent, RecentBudget: a.cfg.RecentTokens * 3 / 2, Memories: mems, Learning: a.st.ListLearning(8),
		Now: a.now().In(a.cfg.Loc), UserText: composed,
	})
	msgs = append(msgs, opts.Tail...)

	a.ensurePlugins(ctx)
	ts := &turnState{chat: chat, deliver: opts.Deliver && a.canSendFiles(), voice: opts.Voice && a.voiceReady()}
	if opts.Deliver {
		ts.notify = func(s string) { a.send(ctx, chat, s) }
	}
	tools, err := a.mergeTools(opts.Tools)
	if err != nil {
		return Reply{}, err
	}
	if ts.voice {
		var arr []json.RawMessage
		if len(tools) > 0 {
			_ = json.Unmarshal(tools, &arr)
		}
		tools, _ = json.Marshal(append(arr, a.voiceToolDef()))
	}
	if ts.voice && a.ttsMode.Load() == ttsWeighted && a.cfg.TTSAdapt {
		msgs = append(msgs, llm.Text("system", leanNote))
	}
	if ts.voice && len(a.cfg.TTSVoices) > 1 {
		msgs = append(msgs, llm.Text("system", a.voiceChoiceNote()))
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
	nudged, warned := false, false
	for {
		limited := rounds >= a.cfg.MaxToolRounds
		if limited {
			msgs = append(msgs, llm.Text("system", finishNow))
		} else if g := min(a.cfg.ToolGraceRounds, a.cfg.MaxToolRounds/2); len(tools) > 0 && !warned && g > 0 && rounds >= a.cfg.MaxToolRounds-g {
			warned = true // once per turn, so the prompt prefix stays stable afterwards
			msgs = append(msgs, llm.Text("system", graceNotice(a.cfg.MaxToolRounds-rounds)))
		}
		if len(tools) > 0 && !limited { // at the cap no tools are offered, so the model can only answer
			res, err = tc.CompleteTools(ctx, msgs, tools, a.cfg.Temperature)
		} else {
			res.Text, err = a.chat.Complete(ctx, msgs, a.cfg.Temperature)
			res.ToolCalls = nil
		}
		if err != nil {
			return Reply{}, err
		}
		if len(res.ToolCalls) == 0 {
			if len(tools) > 0 && !nudged && !limited && promisesAction(res.Text) {
				nudged = true // once per turn; the announcement itself is never shown or stored
				slog.Info("model announced an action without calling a tool; nudging")
				msgs = append(msgs, llm.Message{Role: "assistant", Content: res.Text, ReasoningContent: res.Reasoning}, llm.Text("system", noActionNudge))
				continue
			}
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
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: c.ID, Content: a.runTool(ctx, c, ts)})
		}
		rounds++
	}
	reply := a.rescueVoiceMarker(ctx, res.Text, ts)
	reply, vname := stripVoiceName(reply, a.voiceNames())
	reply, lean := stripLean(reply)
	if vname == "" { // the two markers may come in either order
		reply, vname = stripVoiceName(reply, a.voiceNames())
	}
	if reply == "" && len(ts.spoken) == 0 {
		return Reply{}, errEmpty
	}
	stored := reply
	if len(ts.spoken) > 0 { // keep what she said aloud in the history, so she remembers it
		stored = "（发了语音：" + strings.Join(ts.spoken, " / ") + "）"
		if reply != "" {
			stored += "\n" + reply
		}
	}
	if _, err := a.st.AddMessage(chat, "assistant", stored); err != nil {
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
	return Reply{Text: reply, Files: ts.files, VoiceSent: len(ts.spoken) > 0, VoiceLean: lean, VoiceName: vname}, nil
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
			a.MaybeReview(ctx)
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
	case "/reset":
		if rest != "确认" && rest != "ok" {
			reply("/reset 会让对话从头开始：清空滚动摘要和最近对话窗口，旧对话也不再被提取成记忆。\n" +
				"不会动：已有记忆、核心档案、风格示例，历史消息仍保存在磁盘上。\n" +
				"确定的话发：/reset 确认")
			return
		}
		a.maintMu.Lock()
		n, err := a.st.ResetChat(chat)
		a.maintMu.Unlock()
		if err != nil {
			reply("重置失败：" + truncate(err.Error(), 200))
			return
		}
		reply(fmt.Sprintf("好了，对话已重新开始（清掉了 %d 条未归档的近期消息和摘要）。记忆、核心档案和风格示例都还在。", n))
	case "/style":
		a.styleCommand(ctx, chat, rest, reply)
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
		out := fmt.Sprintf("对话模型：%s\n整理模型：%s\n复盘模型：%s\n读图：%s\n联网搜索：%s\n嵌入模型：%s\n记忆：有效 %d / 已取代 %d / 归档 %d（待重新嵌入 %d）\n风格示例：%d 条\n上次整理：%s",
			a.cfg.LLM.Model, a.cfg.Maint.Model, a.cfg.Review.Model, vision, map[bool]string{true: "开", false: "关"}[a.cfg.SearchEnabled],
			a.emb.Model(), c["active"], c["superseded"], c["archived"],
			a.mm.NeedReembed(), len(a.st.Pins()), last)
		if v := a.style.Active(); v.N > 0 {
			out += fmt.Sprintf("\n风格笔记：v%d（%d 字）", v.N, len([]rune(v.Text)))
		}
		for _, k := range [][2]string{{"chat", "对话"}, {"maint", "后台整理"}, {"vision", "读图"}, {"review", "风格复盘"}} {
			if l := a.describeUsage(k[0], k[1]); l != "" {
				out += "\n累计 " + l
			}
		}
		reply(out)
	case "/plugins":
		if strings.EqualFold(rest, "reload") {
			reply("重新发现中…")
			reply(a.discoverAll(ctx))
			return
		}
		reply(a.pluginStatus())
	case "/voice":
		if a.tts == nil {
			reply("当前没有启用语音回复（设置 TTS_ENABLED=true 以及 TTS_BASE_URL / TTS_MODEL）")
			return
		}
		switch strings.ToLower(rest) {
		case "on", "always":
			a.ttsMode.Store(ttsAlways)
		case "auto":
			a.ttsMode.Store(ttsAuto)
		case "weighted", "mix":
			a.ttsMode.Store(ttsWeighted)
		case "off":
			a.ttsMode.Store(ttsOff)
		case "":
		default:
			reply("用法：/voice on | auto | weighted | off（直接说“发条语音”“别发语音”优先于这个设置）")
			return
		}
		reply(map[int32]string{ttsOff: "语音回复：关闭", ttsAuto: "语音回复：auto（你发语音时我才用语音回）", ttsAlways: "语音回复：总是",
			ttsWeighted: fmt.Sprintf("语音回复：按权重（你发语音时 %.0f%%，发文字时 %.0f%%）", a.cfg.TTSWeightVoice*100, a.cfg.TTSWeightText*100)}[a.ttsMode.Load()] + "（重启后恢复为 TTS_MODE）")
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

// voiceMarkerRe matches the history note "（发了语音：……）" at the start of a reply. The history stores that note for the
// voice messages she sent, and she sometimes copies it into her text instead of calling send_voice.
var voiceMarkerRe = regexp.MustCompile(`(?s)^\s*[（(〔\[]\s*(?:send_voice\s*)?(?:已)?(?:发了?|发出)语音\s*[：:]\s*(.*?)\s*[）)〕\]]\s*`)

// rescueVoiceMarker turns a copied "（发了语音：X）" into what she meant: X is spoken (when she can speak and has not
// already) and the note never reaches the user as text. If speaking is impossible or fails, X is shown as text.
func (a *App) rescueVoiceMarker(ctx context.Context, text string, ts *turnState) string {
	m := voiceMarkerRe.FindStringSubmatch(text)
	if m == nil {
		return text
	}
	inner, rest := strings.TrimSpace(m[1]), strings.TrimSpace(text[len(m[0]):])
	switch {
	case inner == "":
		return rest
	case len(ts.spoken) > 0: // she did call the tool this turn; the note is just noise
		return rest
	case ts.voice && a.speak(ctx, ts.chat, inner, ""):
		slog.Info("voice note written as text instead of calling send_voice; spoke it")
		ts.spoken = append(ts.spoken, inner)
		return rest
	}
	return strings.TrimSpace(inner + "\n" + rest)
}
