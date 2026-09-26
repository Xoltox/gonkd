// Package serial opens and configures the printer's USB-serial port
// (CH340 on /dev/ttyUSB0) for raw, 8N1, 250000 baud communication.
//
// 250000 is not one of the standard POSIX Bxxx termios speeds, so this uses
// the Linux-specific termios2 / BOTHER mechanism (TCGETS2/TCSETS2 ioctls
// with CBAUD replaced by BOTHER and Ispeed/Ospeed set directly) instead of
// cfsetspeed. golang.org/x/sys/unix generates the Termios struct, the ioctl
// numbers and every flag constant per architecture (TCGETS2, TCFLSH and
// IEXTEN differ between mipsle and amd64), so nothing here is hand-coded.
package serial

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Port wraps an open, configured serial device. It implements
// io.ReadWriteCloser and SetReadDeadline so it can be used directly by
// internal/printer.
type Port struct {
	f *os.File
}

// Open opens path (e.g. "/dev/ttyUSB0"), configures it for raw 8N1 at the
// given baud rate using termios2/BOTHER, and returns a ready-to-use Port.
func Open(path string, baud int) (*Port, error) {
	// Deadlines (SetReadDeadline) work because os.OpenFile registers the tty
	// with Go's runtime poller. The rule that keeps it that way: never call
	// f.Fd() on this file. Fd() switches the descriptor back to blocking
	// mode, after which reads ignore deadlines and Close cannot interrupt
	// them. All ioctls go through SyscallConn().Control instead.
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("serial: open %s: %w", path, err)
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("serial: %w", err)
	}

	var t *unix.Termios
	var ioErr error
	if err := rc.Control(func(fd uintptr) { t, ioErr = unix.IoctlGetTermios(int(fd), unix.TCGETS2) }); err != nil {
		ioErr = err
	}
	if ioErr != nil {
		f.Close()
		return nil, fmt.Errorf("serial: TCGETS2: %w", ioErr)
	}

	// Raw mode: no canonical line editing, no echo, no signal chars, no
	// input/output translation, 8 data bits, no parity, one stop bit,
	// ignore modem control lines, enable receiver.
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHONL | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL

	// Keep DTR asserted when the port is closed. With HUPCL set (the
	// default) every close drops DTR, and the next open raises it again;
	// the CH340 board wires DTR to the MCU reset, so a forge restart or
	// reconnect would reset Marlin and kill a running SD print. The tty
	// keeps this setting after close, so only the very first open after
	// the USB device appears produces a DTR edge.
	t.Cflag &^= unix.HUPCL

	// Custom baud rate via BOTHER: clear the CBAUD field (and CBAUDEX,
	// which is folded into CBAUD on Linux) and set it to BOTHER, then give
	// the literal rate via Ispeed/Ospeed.
	t.Cflag &^= unix.CBAUD
	t.Cflag |= unix.BOTHER
	t.Ispeed = uint32(baud)
	t.Ospeed = uint32(baud)

	// VMIN/VTIME: block for at least 1 byte, no inter-byte timeout. (Under
	// the Go poller reads are non-blocking anyway; these only matter to a
	// blocking reader.)
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	if err := rc.Control(func(fd uintptr) { ioErr = unix.IoctlSetTermios(int(fd), unix.TCSETS2, t) }); err != nil {
		ioErr = err
	}
	if ioErr != nil {
		f.Close()
		return nil, fmt.Errorf("serial: TCSETS2: %w", ioErr)
	}

	// Drop whatever arrived before we were listening (stale replies from
	// a previous session, bootloader noise) so the handshake starts clean.
	if err := rc.Control(func(fd uintptr) { ioErr = unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH) }); err != nil {
		ioErr = err
	}
	if ioErr != nil {
		f.Close()
		return nil, fmt.Errorf("serial: TCFLSH: %w", ioErr)
	}

	return &Port{f: f}, nil
}

func (p *Port) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *Port) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *Port) Close() error                { return p.f.Close() }

// SetReadDeadline lets callers (gcode reader, binprotocol client) bound
// individual reads instead of blocking forever on a wedged printer.
func (p *Port) SetReadDeadline(t time.Time) error { return p.f.SetReadDeadline(t) }

// SetDTR toggles the DTR line, which most CH340 boards wire to the MCU's
// reset pin -- so opening the port commonly reboots Marlin. Callers should
// expect a "start" banner after Open and wait for it (see internal/printer).
func (p *Port) SetDTR(on bool) error {
	req := uint(unix.TIOCMBIC)
	if on {
		req = unix.TIOCMBIS
	}
	rc, err := p.f.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	if err := rc.Control(func(fd uintptr) {
		ioErr = unix.IoctlSetPointerInt(int(fd), req, unix.TIOCM_DTR)
	}); err != nil {
		return err
	}
	return ioErr
}
