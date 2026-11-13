package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ahplummer/gostafson/internal/media"
)

func TestMessageWireForm(t *testing.T) {
	plain, _ := json.Marshal(Message{Role: "user", Content: "hi", ImageRefs: []string{"abc.png"}})
	if string(plain) != `{"role":"user","content":"hi"}` {
		t.Errorf("plain = %s", plain)
	}
	withImage, _ := json.Marshal(Message{Role: "user", Content: "what is this?",
		Images: []media.Image{{MediaType: "image/png", Data: []byte{1, 2, 3}}}})
	want := `{"role":"user","content":[{"text":"what is this?","type":"text"},{"image_url":{"url":"data:image/png;base64,AQID"},"type":"image_url"}]}`
	if string(withImage) != want {
		t.Errorf("with image =\n%s\nwant\n%s", withImage, want)
	}
	if strings.Contains(string(withImage), "image_refs") {
		t.Error("image_refs on the wire")
	}
}

func TestVision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		switch {
		case r.URL.Path == "/api/show" && b.Model == "seeing":
			w.Write([]byte(`{"capabilities":["completion","vision","tools"]}`))
		case r.URL.Path == "/api/show":
			w.Write([]byte(`{"capabilities":["completion","tools"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL+"/v1", "")
	if ok, err := c.Vision(context.Background(), "seeing"); !ok || err != nil {
		t.Errorf("seeing = %v, %v", ok, err)
	}
	if ok, err := c.Vision(context.Background(), "blind"); ok || err != nil {
		t.Errorf("blind = %v, %v", ok, err)
	}
	if _, err := New(srv.URL+"/other", "").Vision(context.Background(), "x"); err == nil {
		t.Error("unknown server: no error")
	}
}
