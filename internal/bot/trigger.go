package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	triggerMaxPrompt = 4000
	triggerMaxLabel  = 60
	triggerTimeout   = 10 * time.Minute
)

// apiTrigger starts a turn in the Telegram conversation on behalf of something other than the user (a scheduler plugin,
// cron, a script): POST /v1/trigger {"prompt": "...", "label": "晨报", "chat": 123}. "chat" defaults to the first
// allowed user. It answers 202 at once; the reply (and any files tools produced) goes to Telegram when ready.
func (a *App) apiTrigger(w http.ResponseWriter, r *http.Request) {
	if a.tg == nil {
		apiError(w, 409, "no Telegram bot is configured, so there is nowhere to send the result")
		return
	}
	var req struct {
		Prompt string `json:"prompt"`
		Label  string `json:"label"`
		Chat   *int64 `json:"chat"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		apiError(w, 400, "body must be {\"prompt\": \"...\", \"label\": \"...\"}")
		return
	}
	req.Prompt, req.Label = strings.TrimSpace(req.Prompt), strings.TrimSpace(req.Label)
	switch {
	case req.Prompt == "":
		apiError(w, 400, "prompt is empty")
		return
	case len([]rune(req.Prompt)) > triggerMaxPrompt:
		apiError(w, 400, fmt.Sprintf("prompt is longer than %d characters", triggerMaxPrompt))
		return
	}
	chat := a.defaultChat()
	if req.Chat != nil {
		chat = *req.Chat
	}
	if !a.cfg.AllowedIDs[chat] {
		apiError(w, 403, "chat is not an allowed Telegram user")
		return
	}
	if req.Label == "" {
		req.Label = "定时任务"
	}
	label := truncate(strings.ReplaceAll(req.Label, "\n", " "), triggerMaxLabel)
	slog.Info("scheduled task accepted", "label", label, "chat", chat)
	a.goBG(func() { a.runScheduled(chat, label, req.Prompt) })
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "accepted"})
}

// runScheduled runs one scheduled turn like a normal one (persona, memory, tools, plugins) and sends the result.
func (a *App) runScheduled(chat int64, label, prompt string) {
	l := a.chatLock(chat)
	l.Lock()
	defer l.Unlock()
	a.touch(chat)
	defer a.touch(chat)
	ctx, cancel := context.WithTimeout(a.bg, triggerTimeout)
	defer cancel()
	a.tg.Typing(ctx, chat)
	instruction := fmt.Sprintf("[定时任务「%s」到点了。这是用户事先设好的任务，不是用户刚刚发来的消息。请直接完成它，像平时聊天一样把结果发给用户；"+
		"不要说“收到任务”之类的话，也不要把这段说明复述出来。]\n%s", label, prompt)
	r, err := a.RespondTools(ctx, chat, "", nil, TurnOpts{Instruction: instruction, Deliver: a.canSendFiles()})
	if err != nil {
		slog.Warn("scheduled turn failed", "label", label, "err", err)
		a.send(ctx, chat, fmt.Sprintf("定时任务「%s」没有成功：%s", label, truncate(err.Error(), 200)))
		return
	}
	text := strings.TrimSpace(r.Text)
	if text == "" && len(r.Files) == 0 {
		text = fmt.Sprintf("定时任务「%s」跑完了，但模型没有给出任何内容。", label)
	}
	slog.Info("scheduled task finished", "label", label, "chars", len([]rune(text)), "files", len(r.Files))
	if text != "" {
		a.sendReply(ctx, chat, text)
	}
	a.deliverFiles(ctx, chat, r.Files)
}
