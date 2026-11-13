package lineedit

import (
	"context"
	"encoding/hex"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Clipboard reads the macOS clipboard: a PNG image if there is one (a
// screenshot taken with ⌘⌃⇧4 is), otherwise text. AppleScript returns the
// image as «data PNGf89504E47...», hex-encoded.
func Clipboard(ctx context.Context) (image []byte, text string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "the clipboard as «class PNGf»").Output()
	if err == nil {
		if img, ok := parsePNGf(string(out)); ok {
			return img, "", nil
		}
	}
	t, err := exec.CommandContext(ctx, "/usr/bin/pbpaste").Output()
	if err != nil {
		return nil, "", errors.New("can't read the clipboard")
	}
	return nil, string(t), nil
}

func parsePNGf(s string) ([]byte, bool) {
	i := strings.Index(s, "PNGf")
	if i < 0 {
		return nil, false
	}
	h := s[i+4:]
	end := strings.IndexFunc(h, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F')
	})
	if end >= 0 {
		h = h[:end]
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) < 8 || string(b[1:4]) != "PNG" {
		return nil, false
	}
	return b, true
}
