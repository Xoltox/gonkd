// Package serial opens and configures the printer's USB-serial port
// (CH340 on /dev/ttyUSB0) for raw, 8N1, 250000 baud communication.
//
// 250000 is not one of the standard POSIX Bxxx termios speeds, so this uses
// the Linux-specific termios2 / BOTHER mechanism (TCGETS2/TCSETS2 ioctls
// with CBAUD replaced by BOTHER and Ispeed/Ospeed set directly) instead of
// cfsetspeed. golang.org/x/sys/unix already generates a per-architecture
// Termios struct and TCGETS2/TCSETS2/BOTHER/CBAUD/CBAUDEX ioctl numbers
// (they differ between mipsle and amd64: e.g. TCGETS2 is 0x4030542a on
// mipsle vs 0x802c542a on amd64), so unix.IoctlGetTermios/IoctlSetTermios
// with those request codes work unmodified cross-arch.
//
// ASSUMPTION (documented, not verified against a real MT7628 kernel): the
// non-baud raw-mode flags below (OPOST, ECHO, and the ISTRIP/ICRNL/etc.
// family) use the values from Linux's original/generic termbits layout.
// x/sys/unix's generated per-arch constants confirm CBAUD, CREAD, CLOCAL,
// CS8, CSTOPB, PARENB, ISIG, ICANON, ECHONL and IEXTEN's *presence* differs
// only slightly between mips and amd64 (IEXTEN's bit value differs: 0x100
// vs 0x8000; everything else checked was identical), which is consistent
// with MIPS Linux keeping the "original" POSIX termios bits (which include
// OPOST and ECHO) at the same positions as every other Linux port and only
// relocating the handful of extension flags added later. If raw mode
// behaves oddly on the real box, this is the first thing to re-check.
package serial

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	opost  = 0x1   // Oflag: output processing
	echo   = 0x8   // Lflag: echo input
	ignbrk = 0x1   // Iflag
	brkint = 0x2   // Iflag
	parmrk = 0x8   // Iflag
	istrip = 0x20  // Iflag
	inlcr  = 0x40  // Iflag
	igncr  = 0x80  // Iflag
	icrnl  = 0x100 // Iflag
)

// Port wraps an open, configured serial device. It implements
// io.ReadWriteCloser and SetReadDeadline so it can be used directly by
// internal/binprotocol and internal/gcode readers.
type Port struct {
	f *os.File
}

// Open opens path (e.g. "/dev/ttyUSB0"), configures it for raw 8N1 at the
// given baud rate using termios2/BOTHER, and returns a ready-to-use Port.
func Open(path string, baud int) (*Port, error) {
	// O_NONBLOCK makes os.File register the fd with Go's poller, which is
	// what lets SetReadDeadline work. Never call f.Fd() on it: that flips the
	// descriptor back to blocking mode and deadlines stop firing.
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
	t.Iflag &^= ignbrk | brkint | parmrk | istrip | inlcr | igncr | icrnl | unix.IXON | unix.IXOFF
	t.Oflag &^= opost
	t.Lflag &^= unix.ICANON | echo | unix.ECHONL | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL

	// Custom baud rate via BOTHER: clear the CBAUD field (and CBAUDEX,
	// which is folded into CBAUD on Linux) and set it to BOTHER, then give
	// the literal rate via Ispeed/Ospeed.
	t.Cflag &^= unix.CBAUD
	t.Cflag |= unix.BOTHER
	t.Ispeed = uint32(baud)
	t.Ospeed = uint32(baud)

	// VMIN/VTIME: block for at least 1 byte, no inter-byte timeout; forge's
	// reader always wants complete lines/packets rather than partial reads.
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	if err := rc.Control(func(fd uintptr) { ioErr = unix.IoctlSetTermios(int(fd), unix.TCSETS2, t) }); err != nil {
		ioErr = err
	}
	if ioErr != nil {
		f.Close()
		return nil, fmt.Errorf("serial: TCSETS2: %w", ioErr)
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
