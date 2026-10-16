//go:build darwin || linux

package term

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

type terminal struct {
	*os.File
	saved syscall.Termios
}

// Open opens the terminal with keys unechoed and read one at a time, and Ctrl-C a key
// rather than a signal, so its mode is always restored. Closing it restores the mode. It
// fails when standard output isn't a terminal, as in CI: nobody is there to answer.
func Open() (io.ReadWriteCloser, error) {
	var mode syscall.Termios
	if err := termios(os.Stdout.Fd(), getTermios, &mode); err != nil {
		return nil, err
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	t := &terminal{File: f}
	if err := termios(f.Fd(), getTermios, &t.saved); err != nil {
		f.Close()
		return nil, err
	}
	raw := t.saved
	raw.Iflag &^= syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := termios(f.Fd(), setTermios, &raw); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *terminal) Close() error {
	termios(t.Fd(), setTermios, &t.saved)
	return t.File.Close()
}

func termios(fd, request uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
