// Package speech talks to OpenAI-compatible speech endpoints: /audio/transcriptions (speech to text) and
// /audio/speech (text to speech). Any server that implements those two routes works (whisper.cpp server,
// speaches / faster-whisper-server, Kokoro-FastAPI, openai-edge-tts, ...).
package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"companion/internal/config"
)

type Client struct {
	EP config.Endpoint
	HC *http.Client
}

func New(ep config.Endpoint) *Client { return &Client{EP: ep, HC: &http.Client{}} }

func (c *Client) do(ctx context.Context, req *http.Request) ([]byte, error) {
	timeout := c.EP.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx)
	if c.EP.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.EP.APIKey)
	}
	hc := c.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return b, nil
}

// Transcribe sends one audio file and returns the recognised text. name's extension tells the server the
// container format (Telegram voice notes are .ogg/Opus). language is an ISO code such as "zh"; empty = auto.
func (c *Client) Transcribe(ctx context.Context, audio []byte, name, language string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	_ = w.WriteField("model", c.EP.Model)
	_ = w.WriteField("response_format", "json")
	if language != "" {
		_ = w.WriteField("language", language)
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.EP.BaseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	b, err := c.do(ctx, req)
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		// some servers answer plain text regardless of response_format
		if t := strings.TrimSpace(string(b)); t != "" && !strings.HasPrefix(t, "{") {
			return t, nil
		}
		return "", errors.New("transcription response is not JSON")
	}
	return strings.TrimSpace(out.Text), nil
}

// Synthesize turns text into audio in the given format (mp3, opus, wav...).
func (c *Client) Synthesize(ctx context.Context, text, voice, format string) ([]byte, error) {
	payload := map[string]any{"model": c.EP.Model, "input": text, "response_format": format}
	if voice != "" {
		payload["voice"] = voice
	}
	for k, v := range c.EP.ExtraBody {
		if _, set := payload[k]; !set {
			payload[k] = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.EP.BaseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	b, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("speech endpoint returned no audio")
	}
	return b, nil
}

// CleanForSpeech strips what sounds wrong when read aloud (code blocks, links, markdown marks, LaTeX) and
// returns "" when little speakable text is left. maxRunes <= 0 means no limit; over the limit returns "" too
// (a long answer read aloud is worse than text only).
func CleanForSpeech(s string, maxRunes int) string {
	var out []string
	inCode := false
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
			continue
		}
		if inCode || t == "" {
			continue
		}
		out = append(out, t)
	}
	r := strings.NewReplacer("**", "", "__", "", "`", "", "#", "", ">", "", "$$", "", "$", "", "*", "", "~", "")
	text := r.Replace(strings.Join(out, " "))
	text = stripLinks(text)
	text = strings.Join(strings.Fields(text), " ")
	n := len([]rune(text))
	if n < 2 || (maxRunes > 0 && n > maxRunes) {
		return ""
	}
	return text
}

func stripLinks(s string) string {
	var b strings.Builder
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			continue
		}
		b.WriteString(f)
		b.WriteByte(' ')
	}
	return b.String()
}
