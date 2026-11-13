// Package media prepares images from any transport for a vision model and
// keeps each chat's current image on disk (PRD D34).
//
// Prepare checks the bytes are an image of a supported type and size, and
// scales anything larger than MaxSide down with macOS's sips, so a 12 MP
// phone photo doesn't become 10,000 image tokens. GIF and WebP are converted
// to PNG, which every backend accepts.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // DecodeConfig for GIF
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Limits.
const (
	MaxBytes  = 20 << 20 // largest image accepted from a transport
	MaxSide   = 1568     // longest side sent to the model, in pixels
	MaxImages = 4        // images per message
	// MinSide: thinner images are padded to it. Qwen's vision encoder needs
	// both sides >= 32 px, and Ollama's runner crashes on less (a one-line
	// text screenshot is 724x31).
	MinSide = 64
)

// Image is one prepared image.
type Image struct {
	Name      string // file name from the sender, for notes and the audit log
	MediaType string // image/png or image/jpeg (also gif, webp before Prepare)
	Data      []byte
}

// SHA256 returns the hex digest of the image data.
func (im Image) SHA256() string {
	sum := sha256.Sum256(im.Data)
	return hex.EncodeToString(sum[:])
}

// DataURL returns the image as a data: URL, the form chat APIs take.
func (im Image) DataURL() string {
	return "data:" + im.MediaType + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
}

// Sips is macOS's image tool, used for resizing and conversion.
var Sips = "/usr/bin/sips"

// Prepare checks raw image bytes and returns them ready for a model.
func Prepare(ctx context.Context, name string, data []byte) (Image, error) {
	if len(data) == 0 {
		return Image{}, errors.New("empty image")
	}
	if len(data) > MaxBytes {
		return Image{}, fmt.Errorf("image is %d MB; the limit is %d MB", len(data)>>20, MaxBytes>>20)
	}
	mt := http.DetectContentType(data)
	if isHEIC(data) {
		mt = "image/heic" // iPhone photos; Go doesn't detect it
	}
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/heic":
	default:
		return Image{}, fmt.Errorf("%s isn't a PNG, JPEG, GIF, WebP or HEIC image (%s)", displayName(name), mt)
	}
	im := Image{Name: displayName(name), MediaType: mt, Data: data}
	if mt == "image/gif" || mt == "image/webp" || mt == "image/heic" {
		out, err := sips(ctx, im.Data, mt, true, false)
		if err != nil {
			return Image{}, err
		}
		im.Data, im.MediaType = out, "image/png"
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(im.Data))
	if err != nil {
		return Image{}, fmt.Errorf("%s: can't read the image: %v", im.Name, err)
	}
	if max(cfg.Width, cfg.Height) > MaxSide {
		if out, err := sips(ctx, im.Data, im.MediaType, false, true); err == nil {
			im.Data = out
		} // if scaling fails, send it as is: the server can still read it
		if c, _, err := image.DecodeConfig(bytes.NewReader(im.Data)); err == nil {
			cfg = c
		}
	}
	if min(cfg.Width, cfg.Height) < MinSide {
		out, err := pad(ctx, im.Data, im.MediaType, max(cfg.Width, MinSide), max(cfg.Height, MinSide))
		if err != nil {
			return Image{}, fmt.Errorf("%s is %dx%d, too thin for the model, and padding it failed: %v", im.Name, cfg.Width, cfg.Height, err)
		}
		im.Data = out
	}
	return im, nil
}

// isHEIC reports an ISO media file with a HEIC/HEIF brand ("ftypheic" at
// offset 4).
func isHEIC(b []byte) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	switch string(b[8:12]) {
	case "heic", "heix", "hevc", "heim", "heis", "mif1", "msf1":
		return true
	}
	return false
}

func displayName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		return "image"
	}
	return name
}

// pad centers the image on a white canvas of w x h.
func pad(ctx context.Context, data []byte, mt string, w, h int) ([]byte, error) {
	return runSips(ctx, data, mt, false, "-p", fmt.Sprint(h), fmt.Sprint(w), "--padColor", "FFFFFF")
}

// sips converts and/or scales an image through temporary files.
func sips(ctx context.Context, data []byte, mt string, toPNG, scale bool) ([]byte, error) {
	var args []string
	if toPNG {
		args = append(args, "-s", "format", "png")
	}
	if scale {
		args = append(args, "-Z", fmt.Sprint(MaxSide))
	}
	return runSips(ctx, data, mt, toPNG, args...)
}

func runSips(ctx context.Context, data []byte, mt string, toPNG bool, args ...string) ([]byte, error) {
	if _, err := os.Stat(Sips); err != nil {
		return nil, errors.New("can't resize or convert images here (no sips)")
	}
	dir, err := os.MkdirTemp("", "gostafson-image-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "image/heic": ".heic"}[mt]
	in, out := filepath.Join(dir, "in"+ext), filepath.Join(dir, "out"+ext)
	if toPNG {
		out = filepath.Join(dir, "out.png")
	}
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	args = append(args, in, "--out", out)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if b, err := exec.CommandContext(ctx, Sips, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sips: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return os.ReadFile(out)
}
