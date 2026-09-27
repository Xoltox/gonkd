package printer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Xoltox/gonkd/internal/binprotocol"
	"github.com/Xoltox/gonkd/internal/binprotocol/marlinsim"
	"github.com/Xoltox/gonkd/internal/gcode"
)

// binFake is a Marlin with BINARY_FILE_TRANSFER: a small ASCII host
// protocol (numbered lines, ADVANCED_OK, M115, M20 L, M105, M28 B1) and,
// after "M28 B1", every received byte goes to marlinsim (binary_stream.h)
// until CONTROL CLOSE. Faults: damage flips or drops a byte in 1 of damage
// packets, dropOK loses 1 of dropOK "ok<n>" replies, temps interleaves
// autoreports, mute ignores every binary byte (a printer that switched
// and then stopped answering).
type binFake struct {
	conn net.Conn
	out  chan string
	done chan struct{}
	sim  *marlinsim.Sim

	mu       sync.Mutex
	rng      *rand.Rand
	damage   int
	dropOK   int
	temps    bool
	mute     bool
	starve   time.Duration
	accepted []string // ASCII commands, in order
}

func newBinFake(conn net.Conn, seed uint64) *binFake {
	f := &binFake{
		conn:   conn,
		out:    make(chan string, 1<<16),
		done:   make(chan struct{}),
		rng:    rand.New(rand.NewPCG(seed, 9)),
		starve: 20 * time.Millisecond,
	}
	f.sim = marlinsim.New(f.simOut)
	return f
}

func (f *binFake) send(s string) {
	select {
	case f.out <- s:
	case <-f.done:
	}
}

func (f *binFake) simOut(s string) {
	f.mu.Lock()
	drop := f.dropOK > 0 && strings.HasPrefix(s, "ok") && f.rng.IntN(f.dropOK) == 0
	f.mu.Unlock()
	if !drop {
		f.send(s)
	}
}

func (f *binFake) start() {
	go func() {
		for {
			select {
			case s := <-f.out:
				if _, err := f.conn.Write([]byte(s)); err != nil {
					return
				}
			case <-f.done:
				return
			}
		}
	}()
	go func() {
		t := time.NewTicker(3 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				f.mu.Lock()
				on := f.temps
				f.mu.Unlock()
				if on && f.sim.Binary() {
					f.send(" T:200.00 /200.00 B:60.00 /60.00 @:0 B@:0\n")
				}
			case <-f.done:
				return
			}
		}
	}()
	f.send("start\necho:Marlin 2.1.2.7\n")
	go f.reader()
}

func (f *binFake) stop() {
	f.mu.Lock()
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	f.mu.Unlock()
	f.conn.Close()
}

func (f *binFake) reader() {
	buf := make([]byte, 4096)
	var line []byte
	for {
		if f.sim.InPacket() {
			_ = f.conn.SetReadDeadline(time.Now().Add(f.starve))
		} else {
			_ = f.conn.SetReadDeadline(time.Time{})
		}
		n, err := f.conn.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				f.sim.Starved()
				continue
			}
			f.stop()
			return
		}
		b := append([]byte(nil), buf[:n]...)
		f.mu.Lock()
		mute := f.mute
		if f.sim.Binary() && f.damage > 0 && f.rng.IntN(f.damage) == 0 {
			i := f.rng.IntN(len(b))
			if f.rng.IntN(2) == 0 {
				b[i] ^= 1 << f.rng.IntN(8)
			} else {
				b = append(b[:i], b[i+1:]...)
			}
		}
		f.mu.Unlock()
		for _, c := range b {
			if f.sim.Binary() {
				if !mute {
					f.sim.Byte(c)
				}
				continue
			}
			if c != '\n' {
				line = append(line, c)
				continue
			}
			s := strings.TrimSpace(string(line))
			line = line[:0]
			if s != "" {
				f.handle(s)
			}
		}
	}
}

// handle answers one ASCII line (no sequence check: the ASCII protocol is
// covered by fakeMarlin).
func (f *binFake) handle(s string) {
	ok := "ok\n"
	cmd := s
	if strings.HasPrefix(s, "N") {
		star := strings.LastIndexByte(s, '*')
		sp := strings.IndexByte(s, ' ')
		if star < 0 || sp < 0 {
			f.send("echo:Unknown command: \"" + s + "\"\nok\n")
			return
		}
		cs, _ := strconv.Atoi(s[star+1:])
		if byte(cs) != gcode.Checksum(s[:star]) {
			f.send("Error:checksum mismatch\n")
			return
		}
		cmd = s[sp+1 : star]
		ok = fmt.Sprintf("ok %s P15 B15\n", s[:sp])
	}
	f.mu.Lock()
	f.accepted = append(f.accepted, cmd)
	f.mu.Unlock()
	switch strings.Fields(cmd)[0] {
	case "M115":
		f.send("FIRMWARE_NAME:Marlin 2.1.2.7\nCap:BINARY_FILE_TRANSFER:1\n" + ok)
	case "M20":
		var b strings.Builder
		b.WriteString("Begin file list\n")
		for _, n := range f.sim.Names() {
			data, _ := f.sim.File(n)
			fmt.Fprintf(&b, "%s %d\n", n, len(data))
		}
		b.WriteString("End file list\n")
		f.send(b.String() + ok)
	case "M105":
		f.send("ok T:20.00 /0.00 B:20.00 /0.00 @:0 B@:0\n")
	case "M28":
		if cmd == "M28 B1" {
			f.send("echo:Switching to Binary Protocol\n")
			f.sim.EnterBinary()
		}
		f.send(ok)
	default:
		f.send(ok)
	}
}

func (f *binFake) sawASCII(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.accepted {
		if c == cmd {
			return true
		}
	}
	return false
}

// startBinDriver runs a Driver against a binFake with protocol proto and
// short binary timeouts.
func startBinDriver(t *testing.T, proto UploadProtocol, seed uint64, setup func(*binFake)) (*Driver, *binFake) {
	t.Helper()
	oldProto, oldT, oldO, oldS := UploadProto, binTimeout, binOpenTimeout, binSwitchWait
	UploadProto, binTimeout, binOpenTimeout, binSwitchWait = proto, 50*time.Millisecond, time.Second, time.Second
	t.Cleanup(func() { UploadProto, binTimeout, binOpenTimeout, binSwitchWait = oldProto, oldT, oldO, oldS })

	serverConn, clientConn := net.Pipe()
	fm := newBinFake(serverConn, seed)
	if setup != nil {
		setup(fm)
	}
	fm.start()
	d := New(clientConn, 16)
	if err := d.Handshake(2 * time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	d.Start()
	t.Cleanup(func() {
		d.Close()
		fm.stop()
	})
	waitUntil(t, 2*time.Second, "init commands acked", func() bool {
		return fm.sawASCII("M20 L") && d.window.Outstanding() == 0 && d.Capabilities()["BINARY_FILE_TRANSFER"]
	})
	return d, fm
}

// binTestFile writes a G-code file of about n lines and returns its path
// and what the card must hold afterwards (comment-stripped, as ASCII).
func binTestFile(t *testing.T, n int, seed uint64) (path, want string, lines int) {
	r := rand.New(rand.NewPCG(seed, 3))
	var src, dst strings.Builder
	src.WriteString("; generated by test\n;TIME:1234\nM28 inner.gco\nG28 ; home\n")
	dst.WriteString("G28\n")
	lines = 4
	for i := 0; i < n; i++ {
		l := fmt.Sprintf("G1 X%d.%03d Y%d.5 E%.5f", r.IntN(200), r.IntN(1000), r.IntN(200), r.Float64())
		if i%50 == 0 {
			src.WriteString(";LAYER:" + strconv.Itoa(i/50) + "\n")
			lines++
		}
		src.WriteString(l + " ; move\r\n")
		dst.WriteString(l + "\n")
		lines++
	}
	path = t.TempDir() + "/part.gcode"
	if err := os.WriteFile(path, []byte(src.String()), 0644); err != nil {
		t.Fatal(err)
	}
	return path, dst.String(), lines
}

func TestBinaryUploadExactContentUnderFaults(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		d, fm := startBinDriver(t, ProtoBinary, seed, func(f *binFake) {
			f.damage, f.dropOK, f.temps = 6, 8, true
		})
		path, want, lines := binTestFile(t, 3000, seed)
		var fed int
		d.metaFeed = func(string) { fed++ }
		names := NewNameMap(t.TempDir() + "/names.json")
		var last int64
		short, err := d.UploadToSD(context.Background(), path, "part.gcode", names, func(s, _ int64) { last = s })
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		got, ok := fm.sim.File(short)
		if !ok || string(got) != want {
			t.Fatalf("seed %d: card content differs: %d bytes, want %d", seed, len(got), len(want))
		}
		if fed != lines {
			t.Fatalf("seed %d: metaFeed saw %d lines, want %d", seed, fed, lines)
		}
		if st, _ := os.Stat(path); last != st.Size() {
			t.Fatalf("seed %d: final progress %d of %d", seed, last, st.Size())
		}
		if _, _, rs := fm.sim.Counters(); rs == 0 {
			t.Fatalf("seed %d: no resend requested, faults not exercised", seed)
		}
		if fm.sim.Binary() || d.binMode.Load() {
			t.Fatalf("seed %d: binary mode left on", seed)
		}
		// ASCII works again, and temperatures kept flowing meanwhile.
		if err := d.Send("M117 after"); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 2*time.Second, "ASCII after binary", func() bool { return fm.sawASCII("M117 after") })
		if d.Temps().UpdatedAt.IsZero() {
			t.Fatal("autoreports during the session were not parsed")
		}
	}
}

func TestBinaryUploadCancelAbortsAndDeletes(t *testing.T) {
	d, fm := startBinDriver(t, ProtoBinary, 4, func(f *binFake) { f.dropOK = 10 })
	path, _, _ := binTestFile(t, 3000, 4)
	names := NewNameMap(t.TempDir() + "/names.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := d.UploadToSD(ctx, path, "part.gcode", names, func(s, _ int64) {
		if s > 20000 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := fm.sim.Names(); len(n) != 0 || fm.sim.TransferActive() {
		t.Fatalf("partial file left: %v active=%v", n, fm.sim.TransferActive())
	}
	if fm.sim.Binary() || d.Uploading() {
		t.Fatal("binary mode or upload hold left on")
	}
	if err := d.Send("M117 after"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "ASCII after abort", func() bool { return fm.sawASCII("M117 after") })
}

// A printer that switches and then never answers: the upload fails in
// bounded time, and auto must not fall back to ASCII (Marlin may still be
// reading packets, the ASCII lines would be lost).
func TestBinaryUploadSilentPrinterNoFallback(t *testing.T) {
	d, fm := startBinDriver(t, ProtoAuto, 5, func(f *binFake) { f.mute = true })
	path, _, _ := binTestFile(t, 10, 5)
	start := time.Now()
	_, err := d.UploadToSD(context.Background(), path, "part.gcode", NewNameMap(t.TempDir()+"/n.json"), nil)
	if err == nil || !errors.Is(err, binprotocol.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if fm.sawASCII("M28 PART.GCO") || d.binMode.Load() {
		t.Fatal("fell back to ASCII or left binary mode set")
	}
}

// auto on a firmware that does not switch: M28 B1 opens a file "B1", which
// is closed and deleted, and the upload goes ASCII.
func TestBinaryAutoFallsBackToASCII(t *testing.T) {
	old := UploadProto
	UploadProto = ProtoAuto
	defer func() { UploadProto = old }()
	d, fm := startDriver(t, nil)
	d.mu.Lock()
	d.caps["BINARY_FILE_TRANSFER"] = true // claimed, but M28 B1 opens a file
	d.mu.Unlock()
	dir := t.TempDir()
	path := dir + "/x.gcode"
	if err := os.WriteFile(path, []byte("G28 ; home\nG1 X1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	short, err := d.UploadToSD(context.Background(), path, "x.gcode", NewNameMap(dir+"/n.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	got, b1 := fm.card[short], fm.card["B1"]
	_, hasB1 := fm.card["B1"]
	fm.mu.Unlock()
	if got != "G28\nG1 X1\n" {
		t.Fatalf("content %q", got)
	}
	if hasB1 {
		t.Fatalf("stray file B1 left on the card: %q", b1)
	}
}

func TestParseUploadProtocol(t *testing.T) {
	for _, s := range []string{"ascii", "Binary", " auto "} {
		if _, err := ParseUploadProtocol(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	if _, err := ParseUploadProtocol("zmodem"); err == nil {
		t.Error("zmodem accepted")
	}
}
