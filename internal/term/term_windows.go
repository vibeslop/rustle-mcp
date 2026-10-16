package term

import (
	"io"
	"os"
	"syscall"
)

const (
	enableProcessedInput            = 0x1
	enableLineInput                 = 0x2
	enableEchoInput                 = 0x4
	enableVirtualTerminalInput      = 0x200
	enableProcessedOutput           = 0x1
	enableVirtualTerminalProcessing = 0x4
)

var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

type console struct {
	in, out         *os.File
	inMode, outMode uint32
}

// Open opens the console with keys unechoed and read one at a time, arrows as the escape
// sequences other terminals send, and Ctrl-C a key. Closing it restores the modes. It
// fails when standard output isn't the console, as in CI: nobody is there to answer.
func Open() (io.ReadWriteCloser, error) {
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Handle(os.Stdout.Fd()), &mode); err != nil {
		return nil, err
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		in.Close()
		return nil, err
	}
	c := &console{in: in, out: out}
	err = syscall.GetConsoleMode(handle(in), &c.inMode)
	if err == nil {
		err = syscall.GetConsoleMode(handle(out), &c.outMode)
	}
	if err == nil {
		// Consoles older than Windows 10 refuse these modes, and get no checklist.
		err = set(out, c.outMode|enableProcessedOutput|enableVirtualTerminalProcessing)
	}
	if err == nil {
		if err = set(in, c.inMode&^(enableProcessedInput|enableLineInput|enableEchoInput)|enableVirtualTerminalInput); err != nil {
			set(out, c.outMode)
		}
	}
	if err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	return c, nil
}

func (c *console) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *console) Write(p []byte) (int, error) { return c.out.Write(p) }

func (c *console) Close() error {
	set(c.in, c.inMode)
	set(c.out, c.outMode)
	c.in.Close()
	return c.out.Close()
}

func handle(f *os.File) syscall.Handle { return syscall.Handle(f.Fd()) }

func set(f *os.File, mode uint32) error {
	if ok, _, err := setConsoleMode.Call(f.Fd(), uintptr(mode)); ok == 0 {
		return err
	}
	return nil
}
