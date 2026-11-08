package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"companion/internal/telegram"
)

// Files the user sends (anything that is not an image) are handed to the plugin servers. The first server that accepts
// files stores the file somewhere useful (mnemo-plugin: the dev container's inbox) and says where; the model is then told.
//
//	POST {base}/v1/files  <- {"name":"a.zip","mime":"application/zip","data":"<base64>","context":{"chat":123,"channel":"telegram","tz":"..."}}
//	                      -> {"text":"saved to /workspace/ai/inbox/a.zip"}  or {"error":"..."}
//	                         404 means "this server does not take files"; the next server is tried.
const pluginMaxUpload = 20 << 20 // Telegram does not let bots download more than this

var errNoReceiver = errors.New("收到文件了，但没有插件愿意接收（需要启用 mnemo-plugin 的 dev_exec，即设置 DEV_SSH_HOST）")

// forwardFile offers a file to each plugin server in turn and returns the receiver's description of where it went.
func (a *App) forwardFile(ctx context.Context, chat int64, name, mime string, data []byte) (string, error) {
	var urls []string
	for u := range a.plug.urls {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	body, _ := json.Marshal(map[string]any{"name": name, "mime": mime, "data": data, // []byte is base64 in JSON
		"context": map[string]any{"chat": chat, "channel": "telegram", "tz": a.cfg.Loc.String()}})
	for _, base := range urls {
		cctx, cancel := context.WithTimeout(ctx, a.cfg.PluginTimeout)
		text, accepted, err := a.postFile(cctx, base, body)
		cancel()
		if err != nil {
			slog.Warn("plugin refused a file", "plugin", base, "err", err)
			return "", err
		}
		if accepted {
			return text, nil
		}
	}
	return "", errNoReceiver
}

func (a *App) postFile(ctx context.Context, base string, body []byte) (text string, accepted bool, err error) {
	req, err := a.pluginReq(ctx, http.MethodPost, base+"/v1/files", body)
	if err != nil {
		return "", false, err
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("插件连不上：%w", cleanNetErr(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &out)
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return "", false, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", false, fmt.Errorf("插件拒绝了请求（HTTP %d）：PLUGIN_TOKEN 不正确？", resp.StatusCode)
	case resp.StatusCode != http.StatusOK || out.Error != "":
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", false, errors.New(truncate(msg, 200))
	}
	return strings.TrimSpace(out.Text), true, nil
}

// receiveDoc downloads a document the user sent, hands it to a plugin and returns the text of the turn: the user's caption
// followed by a note saying where the file went. ok is false when the user has already been told why nothing happens.
func (a *App) receiveDoc(ctx context.Context, chat int64, d *telegram.Document, caption string) (text string, ok bool) {
	a.tg.Typing(ctx, chat)
	name := d.FileName
	if name == "" {
		name = "file"
	}
	if d.FileSize > pluginMaxUpload {
		a.send(ctx, chat, fmt.Sprintf("文件太大（%.1f MB），Telegram 机器人最多只能收 20 MB。", float64(d.FileSize)/(1<<20)))
		return "", false
	}
	data, err := a.tg.Download(ctx, d.FileID)
	if err != nil {
		a.send(ctx, chat, "下载文件失败："+truncate(err.Error(), 200))
		return "", false
	}
	saved, err := a.forwardFile(ctx, chat, name, d.MimeType, data)
	if err != nil {
		a.send(ctx, chat, "保存文件失败："+truncate(err.Error(), 200))
		return "", false
	}
	a.send(ctx, chat, "📥 "+saved) // a receipt that does not depend on the model answering
	note := fmt.Sprintf("（用户发送了文件「%s」。%s）", name, saved)
	if caption = strings.TrimSpace(caption); caption != "" {
		return caption + "\n\n" + note, true
	}
	return note, true
}
