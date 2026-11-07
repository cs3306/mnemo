package telegram

import (
	"context"
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
