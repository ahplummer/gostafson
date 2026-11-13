//go:build !darwin

package lineedit

import "errors"

// MakeRaw is only implemented for macOS, gostafson's one platform; other
// systems fall back to plain line input.
func MakeRaw(fd uintptr) (func(), error) {
	return nil, errors.New("raw mode: not supported on this OS")
}

// Width returns 80 on other systems.
func Width(fd uintptr) int { return 80 }
