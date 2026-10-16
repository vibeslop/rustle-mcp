//go:build !darwin && !linux && !windows

package term

import (
	"errors"
	"io"
)

// Open fails here, so setup adds Rustle to every client found, as it does in scripts.
func Open() (io.ReadWriteCloser, error) {
	return nil, errors.New("no raw terminal on this system")
}
