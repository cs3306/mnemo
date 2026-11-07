package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorsNeverContainToken(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, token) {
			t.Errorf("token missing from path")
		}
		http.Error(w, "nope", 500)
	}))
	url := srv.URL
	c := New(token, url)
	err := c.Send(context.Background(), 1, "hi")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("http error leaks token: %v", err)
	}
	srv.Close() // connection-refused path produces a *url.Error
	err = c.Send(context.Background(), 1, "hi")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("network error leaks token: %v", err)
	}
	_, err = c.Download(context.Background(), "f")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("download error leaks token: %v", err)
	}
}

func TestSendFileUsesPhotoForSmallImagesAndDocumentOtherwise(t *testing.T) {
	var methods []string
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		for field, fhs := range r.MultipartForm.File {
			f, _ := fhs[0].Open()
			b, _ := io.ReadAll(f)
			got = append(got, field+":"+fhs[0].Filename+":"+string(b)+":chat="+r.FormValue("chat_id"))
		}
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	c := New("1:T", srv.URL)
	if err := c.SendFile(context.Background(), 42, "a.png", "image/png", []byte("PNG")); err != nil {
		t.Fatal(err)
	}
	if err := c.SendFile(context.Background(), 42, "报告.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("DOCX")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "sendPhoto,sendDocument" {
		t.Fatalf("%v", methods)
	}
	if got[0] != "photo:a.png:PNG:chat=42" || got[1] != "document:报告.docx:DOCX:chat=42" {
		t.Fatalf("%v", got)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"description":"file is too big"}`))
	}))
	defer bad.Close()
	err := New("1:SECRET", bad.URL).SendFile(context.Background(), 1, "x", "text/plain", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "too big") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}
