//go:build !unix

package proc

import "os"

// Processes don't start warm here (warmEnabled), so nothing calls these.

func alive(int) bool { return true }

func owned(os.FileInfo) bool { return false }

func terminate(p *process) { p.kill() }
