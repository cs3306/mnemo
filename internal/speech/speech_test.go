package speech

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"companion/internal/config"
)

func TestTranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file", 400)
			return
		}
		data, _ := io.ReadAll(f)
		if string(data) != "OGG" || hdr.Filename != "voice.ogg" || r.FormValue("model") != "whisper" || r.FormValue("language") != "zh" {
			http.Error(w, "fields", 400)
			return
		}
		w.Write([]byte(`{"text":" 你好 "}`))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "whisper", Timeout: 5 * time.Second})
	got, err := c.Transcribe(context.Background(), []byte("OGG"), "voice.ogg", "zh")
	if err != nil || got != "你好" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestSynthesize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if r.URL.Path != "/v1/audio/speech" || p["input"] != "hi" || p["voice"] != "v" || p["response_format"] != "mp3" || p["speed"] != 1.1 {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte("MP3DATA"))
	}))
	defer srv.Close()
	c := New(config.Endpoint{BaseURL: srv.URL + "/v1", Model: "tts", ExtraBody: map[string]any{"speed": 1.1}})
	b, err := c.Synthesize(context.Background(), "hi", "v", "mp3")
	if err != nil || string(b) != "MP3DATA" {
		t.Fatalf("got %q err %v", b, err)
	}
	if _, err := New(config.Endpoint{BaseURL: srv.URL + "/v1"}).Synthesize(context.Background(), "x", "", "mp3"); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestCleanForSpeech(t *testing.T) {
	in := "**好的**，看这个：\n```go\nfmt.Println(1)\n```\n详见 https://example.com 公式 $x^2$。"
	got := CleanForSpeech(in, 100)
	if strings.ContainsAny(got, "*`$") || strings.Contains(got, "http") || strings.Contains(got, "Println") {
		t.Fatalf("not cleaned: %q", got)
	}
	if !strings.Contains(got, "好的") {
		t.Fatalf("lost text: %q", got)
	}
	if CleanForSpeech(strings.Repeat("长", 50), 10) != "" || CleanForSpeech("```\ncode\n```", 100) != "" {
		t.Fatal("expected empty")
	}
}
