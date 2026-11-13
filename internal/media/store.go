package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Store keeps each chat's current images: the ones from the latest message
// that had any. They are re-sent with follow-up messages until a new image
// arrives or the chat is reset (PRD D34). Files are named by their SHA-256
// and live in one directory per chat, mode 0600.
type Store struct {
	dir string
}

// OpenStore returns a store rooted at dir (created 0700).
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) chatDir(chat string) string {
	sum := sha256.Sum256([]byte(chat)) // chat IDs hold ":" and phone or group IDs
	return filepath.Join(s.dir, hex.EncodeToString(sum[:8]))
}

// Ref names an image in the store: its SHA-256 plus extension.
func Ref(im Image) string {
	ext := ".png"
	if im.MediaType == "image/jpeg" {
		ext = ".jpg"
	}
	return im.SHA256() + ext
}

// SetCurrent replaces chat's current images and returns their refs.
func (s *Store) SetCurrent(chat string, ims []Image) ([]string, error) {
	dir := s.chatDir(chat)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var refs []string
	for _, im := range ims {
		ref := Ref(im)
		if err := os.WriteFile(filepath.Join(dir, ref), im.Data, 0o600); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// Current loads the chat's current images for the given refs. A ref that
// isn't current (an older image, or one cleared by a reset) is skipped.
func (s *Store) Current(chat string, refs []string) []Image {
	var out []Image
	for _, ref := range refs {
		if strings.ContainsAny(ref, `/\`) || ref == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.chatDir(chat), ref))
		if err != nil {
			continue
		}
		mt := "image/png"
		if strings.HasSuffix(ref, ".jpg") {
			mt = "image/jpeg"
		}
		out = append(out, Image{Name: ref, MediaType: mt, Data: b})
	}
	return out
}

// Reset deletes chat's images.
func (s *Store) Reset(chat string) error {
	err := os.RemoveAll(s.chatDir(chat))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
