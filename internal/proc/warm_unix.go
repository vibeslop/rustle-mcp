//go:build unix

package proc

import (
	"os"
	"syscall"
)

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	// EPERM: alive, and someone else's.
	return err == nil || err == syscall.EPERM
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func terminate(p *process) { _ = p.cmd.Process.Signal(syscall.SIGTERM) }
