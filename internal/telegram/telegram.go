// Package telegram is a minimal Telegram Bot API client using long polling (outbound HTTPS only).
// The bot token is part of every request URL, so errors are stripped of the URL before they escape.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"companion/internal/textutil"
)

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	Chat     Chat        `json:"chat"`
	From     *User       `json:"from"`
	Text     string      `json:"text"`
	Caption  string      `json:"caption"`
	Photo    []PhotoSize `json:"photo"`
	Document *Document   `json:"document"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type User struct {
	ID int64 `json:"id"`
}

type PhotoSize struct {
	FileID string `json:"file_id"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type Client struct {
	apiBase string
	token   string
	hc      *http.Client
	richOff atomic.Bool // set once the server answers 404 for sendRichMessage (older/self-hosted Bot API)
}

// APIError is an answer from the Bot API server itself (as opposed to a network failure): the request was
// seen and rejected, so it is safe to retry the same content another way.
type APIError struct {
	Method string
	Code   int
	Desc   string
}

func (e *APIError) Error() string { return fmt.Sprintf("telegram %s: %s", e.Method, e.Desc) }

func New(token, apiBase string) *Client {
	return &Client{apiBase: apiBase, token: token, hc: &http.Client{Timeout: 90 * time.Second}}
}

// sanitize drops the *url.Error wrapper, whose message contains the full URL (and therefore the token).
func sanitize(method string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("telegram %s: %w", method, err)
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/%s", c.apiBase, c.token, method), bytes.NewReader(body))
	if err != nil {
		return sanitize(method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return sanitize(method, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return sanitize(method, err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return &APIError{Method: method, Code: resp.StatusCode, Desc: fmt.Sprintf("non-JSON response (HTTP %d)", resp.StatusCode)}
	}
	if !env.OK {
		return &APIError{Method: method, Code: env.ErrorCode, Desc: env.Description}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (c *Client) Updates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeoutSec, "allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}

// Send posts plain text (model output contains LaTeX / underscores that break Markdown parse modes).
func (c *Client) Send(ctx context.Context, chat int64, text string) error {
	for _, part := range textutil.SplitMessage(text, 3500) {
		if err := c.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": part}, nil); err != nil {
			return err
		}
	}
	return nil
}

// SendRich posts model output as a rich message (Bot API 10.1+): headings, lists, tables and LaTeX formulas
// render natively. Each chunk is sent with sendRichMessage; if the server rejects it (bad markup, or a
// server that does not know the method) that chunk is re-sent as plain text, so a reply is never lost.
// Network errors are returned as-is without a retry, to avoid duplicating a message that may have arrived.
func (c *Client) SendRich(ctx context.Context, chat int64, text string) error {
	for _, part := range textutil.SplitMessage(text, 6000) {
		if !c.richOff.Load() {
			err := c.call(ctx, "sendRichMessage", map[string]any{
				"chat_id":      chat,
				"rich_message": map[string]any{"markdown": PrepareRich(part)},
			}, nil)
			if err == nil {
				continue
			}
			var ae *APIError
			if !errors.As(err, &ae) {
				return err
			}
			if ae.Code == http.StatusNotFound {
				c.richOff.Store(true)
			}
		}
		if err := c.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": part}, nil); err != nil {
			return err
		}
	}
	return nil
}

// BotCommand is one entry of the "/" menu Telegram shows next to the message box.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetCommands publishes the command menu for one private chat (so only that user sees it).
func (c *Client) SetCommands(ctx context.Context, chat int64, cmds []BotCommand) error {
	return c.call(ctx, "setMyCommands", map[string]any{
		"commands": cmds, "scope": map[string]any{"type": "chat", "chat_id": chat},
	}, nil)
}

// SendFile uploads a file as a photo (small images) or a document. name is shown to the recipient.
func (c *Client) SendFile(ctx context.Context, chat int64, name, mime string, data []byte) error {
	method, field := "sendDocument", "document"
	if (mime == "image/png" || mime == "image/jpeg") && len(data) <= 9<<20 {
		method, field = "sendPhoto", "photo"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", fmt.Sprint(chat))
	fw, err := w.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/%s", c.apiBase, c.token, method), &buf)
	if err != nil {
		return sanitize(method, err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := (&http.Client{Timeout: 180 * time.Second}).Do(req)
	if err != nil {
		return sanitize(method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("telegram %s: non-JSON response (HTTP %d)", method, resp.StatusCode)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	return nil
}

func (c *Client) Typing(ctx context.Context, chat int64) {
	_ = c.call(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
}

func (c *Client) Download(ctx context.Context, fileID string) ([]byte, error) {
	const maxBytes = 20_000_000 // the Bot API's own download limit
	var info struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &info); err != nil {
		return nil, err
	}
	if info.FileSize > maxBytes {
		return nil, errors.New("file too large")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/file/bot%s/%s", c.apiBase, c.token, info.FilePath), nil)
	if err != nil {
		return nil, sanitize("download", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, sanitize("download", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}
