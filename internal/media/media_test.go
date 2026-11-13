package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"testing"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func size(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestPrepare(t *testing.T) {
	ctx := context.Background()
	small := pngOf(t, 200, 100)
	im, err := Prepare(ctx, "/tmp/shot.png", small)
	if err != nil || im.MediaType != "image/png" || !bytes.Equal(im.Data, small) || im.Name != "shot.png" {
		t.Fatalf("small png: %v %s %q", err, im.MediaType, im.Name)
	}
	if _, err := Prepare(ctx, "x.txt", []byte("hello, not an image")); err == nil {
		t.Error("text accepted as an image")
	}
	if _, err := Prepare(ctx, "x", nil); err == nil {
		t.Error("empty accepted")
	}
	if _, err := Prepare(ctx, "x", make([]byte, MaxBytes+1)); err == nil {
		t.Error("oversized accepted")
	}
	if _, err := os.Stat(Sips); err != nil {
		t.Skip("no sips")
	}

	big, err := Prepare(ctx, "big.png", pngOf(t, 4000, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := size(t, big.Data); w != MaxSide || h != MaxSide/4 {
		t.Errorf("big png scaled to %dx%d", w, h)
	}

	var j bytes.Buffer
	jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, 3000, 2000)), nil)
	ji, err := Prepare(ctx, "photo.jpg", j.Bytes())
	if err != nil || ji.MediaType != "image/jpeg" {
		t.Fatalf("jpeg: %v %s", err, ji.MediaType)
	}
	if w, _ := size(t, ji.Data); w != MaxSide {
		t.Errorf("jpeg width %d", w)
	}

	var g bytes.Buffer
	gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 50, 40), []color.Color{color.Black, color.White}), nil)
	gi, err := Prepare(ctx, "anim.gif", g.Bytes())
	if err != nil || gi.MediaType != "image/png" {
		t.Fatalf("gif: %v %s", err, gi.MediaType)
	}
	if w, h := size(t, gi.Data); w != 50 || h != 40 {
		t.Errorf("gif converted to %dx%d", w, h) // converted, not scaled up
	}
}

func TestStore(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := Image{MediaType: "image/png", Data: []byte("a")}
	b := Image{MediaType: "image/jpeg", Data: []byte("b")}
	refsA, _ := s.SetCurrent("tg:1", []Image{a})
	if got := s.Current("tg:1", refsA); len(got) != 1 || string(got[0].Data) != "a" {
		t.Fatalf("current = %v", got)
	}
	if got := s.Current("tg:2", refsA); len(got) != 0 {
		t.Error("another chat sees the image")
	}
	refsB, _ := s.SetCurrent("tg:1", []Image{b})
	if got := s.Current("tg:1", refsA); len(got) != 0 {
		t.Error("older image still current")
	}
	if got := s.Current("tg:1", refsB); len(got) != 1 || got[0].MediaType != "image/jpeg" {
		t.Errorf("new image = %v", got)
	}
	if got := s.Current("tg:1", []string{"../../etc/passwd"}); len(got) != 0 {
		t.Error("path in ref")
	}
	s.Reset("tg:1")
	if got := s.Current("tg:1", refsB); len(got) != 0 {
		t.Error("image survived reset")
	}
}
