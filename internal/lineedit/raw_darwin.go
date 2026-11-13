//go:build darwin

package lineedit

import (
	"syscall"
	"unsafe"
)

// MakeRaw puts the terminal on fd into raw input mode (keys arrive one at a
// time, no echo, Ctrl-C as a byte) and returns a function that restores it.
// Output processing stays on, so "\n" still starts a new line.
func MakeRaw(fd uintptr) (restore func(), err error) {
	var old syscall.Termios
	if err := ioctl(fd, syscall.TIOCGETA, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TIOCSETA, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() { ioctl(fd, syscall.TIOCSETA, unsafe.Pointer(&old)) }, nil
}

// Width returns the terminal's width in columns, or 80 if unknown.
func Width(fd uintptr) int {
	var ws struct{ Row, Col, X, Y uint16 }
	if ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}
