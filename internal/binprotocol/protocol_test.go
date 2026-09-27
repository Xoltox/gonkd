package binprotocol_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Xoltox/gonkd/internal/binprotocol"
	"github.com/Xoltox/gonkd/internal/binprotocol/marlinsim"
	"github.com/Xoltox/gonkd/internal/heatshrink"
)

// Golden packets from Marlin's reference client,
// buildroot/share/scripts/MarlinBinaryProtocol.py Protocol.build_packet.
func TestPacketGolden(t *testing.T) {
	seq := make([]byte, 96)
	for i := range seq {
		seq[i] = byte(i)
	}
	cases := []struct {
		name        string
		sync, p, ty uint8
		payload     []byte
		want        string
	}{
		{"sync", 0, 0, 1, nil, "adb5000100000103"},
		{"query", 1, 1, 0, nil, "adb5011000001134"},
		{"open", 2, 1, 1, []byte("\x00\x01TEST.GCO\x00"), "adb502110b001e510001544553542e47434f00d709"},
		{"write96", 3, 1, 3, seq, "adb5031360007606" + hex.EncodeToString(seq) + "d433"},
		{"close", 255, 1, 2, nil, "adb5ff1200001236"},
		{"control close", 7, 0, 2, nil, "adb5070200000922"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(binprotocol.Packet(c.sync, c.p, c.ty, c.payload))
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestParseReply(t *testing.T) {
	cases := []struct {
		in   string
		kind binprotocol.ReplyKind
		sync uint8
	}{
		{"ok12\n", binprotocol.ReplyOK, 12},
		{"rs0", binprotocol.ReplyResend, 0},
		{"fe255", binprotocol.ReplyFatal, 255},
		{"ss3,96,0.1.0", binprotocol.ReplySync, 3},
		{"PFT:success", binprotocol.ReplyPFT, 0},
		{"PTF:invalid", binprotocol.ReplyPFT, 0},
		{"ok N12 P15 B3", binprotocol.ReplyNone, 0},
		{"ok T:20.0 /0.0", binprotocol.ReplyNone, 0},
		{"ok", binprotocol.ReplyNone, 0},
		{"ok256", binprotocol.ReplyNone, 0},
		{"echo:Resend request 1", binprotocol.ReplyNone, 0},
		{" T:200.00 /200.00 B:60.00 /60.00 @:0 B@:0", binprotocol.ReplyNone, 0},
	}
	for _, c := range cases {
		r, ok := binprotocol.ParseReply(c.in)
		if c.kind == binprotocol.ReplyNone {
			if ok {
				t.Errorf("%q parsed as %+v", c.in, r)
			}
			continue
		}
		if !ok || r.Kind != c.kind || r.Sync != c.sync {
			t.Errorf("%q: got %+v ok=%v", c.in, r, ok)
		}
	}
	r, _ := binprotocol.ParseReply("ss7,96,0.1.0")
	if r.BufSize != 96 || r.Version != "0.1.0" {
		t.Errorf("ss fields: %+v", r)
	}
}

// link joins a Session to a Sim with the faults the tests ask for. Bytes
// go through a goroutine that plays Marlin's main loop: a packet left
// incomplete for starve is PACKET_TIMEOUT.
type link struct {
	sim   *marlinsim.Sim
	lines chan string
	rx    chan []byte

	mu      sync.Mutex
	rng     *rand.Rand
	damage  int // 1 in damage packets gets a byte flipped or lost
	dropOK  int // 1 in dropOK "ok<n>" lines is lost
	extraRS int // 1 in extraRS accepted packets also draws a spurious rs
	chatter bool
	starve  time.Duration
	trace   []string // last lines and packets, for failure messages
	wire    int
}

func newLink(t *testing.T, seed uint64) *link {
	l := &link{
		lines:  make(chan string, 1<<14),
		rx:     make(chan []byte, 1024),
		rng:    rand.New(rand.NewPCG(seed, 1)),
		starve: 10 * time.Millisecond,
	}
	l.sim = marlinsim.New(l.out)
	l.sim.EnterBinary()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go l.run(done)
	return l
}

func (l *link) out(s string) {
	l.mu.Lock()
	l.note("RX " + strings.TrimSpace(s))
	drop := l.dropOK > 0 && strings.HasPrefix(s, "ok") && l.rng.IntN(l.dropOK) == 0
	extra := l.extraRS > 0 && strings.HasPrefix(s, "ok") && l.rng.IntN(l.extraRS) == 0
	chatter := l.chatter
	l.mu.Unlock()
	if chatter {
		l.lines <- " T:200.00 /200.00 B:60.00 /60.00 @:0 B@:0\n"
	}
	if !drop {
		l.lines <- s
	}
	if extra {
		// A resend request for the packet after the one just acked, as
		// Marlin sends when line noise looked like a packet start.
		if n, err := strconv.Atoi(strings.TrimSpace(s[2:])); err == nil {
			l.lines <- fmt.Sprintf("rs%d\n", uint8(n+1))
		}
	}
}

func (l *link) write(p []byte) error {
	b := append([]byte(nil), p...)
	l.mu.Lock()
	l.wire += len(b)
	l.note(fmt.Sprintf("TX sync=%d meta=%02x len=%d", b[2], b[3], len(b)))
	if l.damage > 0 && l.rng.IntN(l.damage) == 0 {
		i := l.rng.IntN(len(b))
		if l.rng.IntN(2) == 0 {
			b[i] ^= 1 << l.rng.IntN(8)
		} else {
			b = append(b[:i], b[i+1:]...)
		}
	}
	l.mu.Unlock()
	l.rx <- b
	return nil
}

func (l *link) run(done chan struct{}) {
	for {
		var t <-chan time.Time
		if l.sim.InPacket() {
			t = time.After(l.starve)
		}
		select {
		case b := <-l.rx:
			for _, c := range b {
				l.sim.Byte(c)
			}
		case <-t:
			l.sim.Starved()
		case <-done:
			return
		}
	}
}

func (l *link) cfg() binprotocol.Config {
	return binprotocol.Config{
		Write:       l.write,
		Lines:       l.lines,
		Timeout:     40 * time.Millisecond,
		OpenTimeout: 500 * time.Millisecond,
		Retries:     50,
	}
}

func gcodeLike(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 2))
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		switch r.IntN(4) {
		case 0:
			b.WriteString("G1 X12.5 Y33.1 E0.0421\n")
		case 1:
			b.WriteString("G1 F1800 X" + string(rune('0'+r.IntN(10))) + "3.07 Y81.2\n")
		case 2:
			b.WriteByte(byte(r.IntN(256)))
			b.WriteString(";TYPE:WALL-OUTER\n")
		default:
			b.WriteString("M204 S1000\n")
		}
	}
	return b.Bytes()
}

func upload(ctx context.Context, l *link, name string, data []byte, window int, compress bool) (*binprotocol.Session, error) {
	cfg := l.cfg()
	cfg.Window = window
	s, err := binprotocol.Start(ctx, cfg)
	if err != nil {
		return s, err
	}
	if s.MaxPayload() != 96 {
		return s, errors.New("buffer size not learned")
	}
	if _, _, ok := s.Heatshrink(); !ok && compress {
		return s, errors.New("heatshrink not reported")
	}
	if err := s.Open(ctx, name, compress); err != nil {
		return s, err
	}
	payload := data
	if compress {
		payload, err = heatshrink.Encode(data, heatshrink.DefaultConfig())
		if err != nil {
			return s, err
		}
	}
	for len(payload) > 0 {
		n := min(len(payload), 250)
		if err := s.Write(ctx, payload[:n]); err != nil {
			return s, err
		}
		payload = payload[n:]
	}
	if err := s.Close(ctx); err != nil {
		return s, err
	}
	return s, s.Exit(ctx)
}

func TestSessionCleanTransfer(t *testing.T) {
	for _, w := range []int{1, 4} {
		for _, comp := range []bool{false, true} {
			l := newLink(t, 1)
			data := gcodeLike(3000, 1)
			s, err := upload(context.Background(), l, "TEST.GCO", data, w, comp)
			if err != nil {
				t.Fatalf("window %d compress %v: %v", w, comp, err)
			}
			got, _ := l.sim.File("TEST.GCO")
			if !bytes.Equal(got, data) {
				t.Fatalf("window %d compress %v: content differs (%d vs %d bytes)", w, comp, len(got), len(data))
			}
			if l.sim.Binary() {
				t.Fatal("still in binary mode after Exit")
			}
			if st := s.Stats(); st.Resends != 0 {
				t.Fatalf("resends on a clean link: %+v", st)
			}
		}
	}
}

// Every fault at once, many seeds: flipped and lost bytes (header and
// payload corruption, lost tokens, starvation), lost oks, spurious resend
// requests and autoreports between replies. The file must arrive exact,
// the sync must wrap past 255 and the session must end in ASCII mode.
func TestSessionSurvivesFaults(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		for _, w := range []int{1, 4} {
			l := newLink(t, seed)
			l.damage, l.dropOK, l.extraRS, l.chatter = 7, 9, 23, true
			data := gcodeLike(1500, seed)
			comp := seed%2 == 0
			s, err := upload(context.Background(), l, "FAULT.GCO", data, w, comp)
			if err != nil {
				t.Fatalf("seed %d window %d: %v (stats %+v)\n%s", seed, w, err, s.Stats(), l.dump())
			}
			got, _ := l.sim.File("FAULT.GCO")
			if !bytes.Equal(got, data) {
				t.Fatalf("seed %d window %d: content differs (%d vs %d bytes)", seed, w, len(got), len(data))
			}
			if l.sim.Binary() {
				t.Fatalf("seed %d: still in binary mode", seed)
			}
			if _, _, rs := l.sim.Counters(); rs == 0 {
				t.Fatalf("seed %d: no resend was requested; faults not exercised", seed)
			}
		}
	}
}

func TestSessionAbortDeletesFile(t *testing.T) {
	l := newLink(t, 3)
	l.sim.PutFile("OLD.GCO", []byte("G28\n"))
	s, err := binprotocol.Start(context.Background(), l.cfg())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Open(ctx, "PART.GCO", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, gcodeLike(50, 3)); err != nil {
		t.Fatal(err)
	}
	// Abort with packets still in flight, then leave binary mode. The
	// second Abort, with no file open, must not send ABORT: that would
	// delete card.filename.
	for i := 0; i < 2; i++ {
		if err := s.Abort(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Exit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.sim.File("PART.GCO"); ok || l.sim.TransferActive() {
		t.Fatal("partial file left on the card")
	}
	if _, ok := l.sim.File("OLD.GCO"); !ok {
		t.Fatal("abort deleted another file")
	}
	if l.sim.Binary() {
		t.Fatal("still in binary mode")
	}
	if _, aborts, _ := l.sim.Counters(); aborts != 1 {
		t.Fatalf("aborts = %d, want 1", aborts)
	}
}

func TestSessionOpenFailAndTimeout(t *testing.T) {
	l := newLink(t, 4)
	l.sim.FailOpen = true
	s, err := binprotocol.Start(context.Background(), l.cfg())
	if err != nil {
		t.Fatal(err)
	}
	var pe *binprotocol.PFTError
	if err := s.Open(context.Background(), "X.GCO", false); !errors.As(err, &pe) || pe.Status != "fail" {
		t.Fatalf("open error = %v", err)
	}

	// Nobody answering: Start gives up with ErrTimeout.
	dead := binprotocol.Config{Write: func([]byte) error { return nil }, Lines: make(chan string), Timeout: 10 * time.Millisecond}
	if _, err := binprotocol.Start(context.Background(), dead); !errors.Is(err, binprotocol.ErrTimeout) {
		t.Fatalf("start with no printer: %v", err)
	}
}

func TestSessionRespectsReportedBufSize(t *testing.T) {
	l := newLink(t, 5)
	l.sim.BufSize = 32 // ss reports 32; pretend we ignored it
	s, err := binprotocol.Start(context.Background(), l.cfg())
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxPayload() != 32 {
		t.Fatalf("MaxPayload = %d", s.MaxPayload())
	}
	if err := s.Open(context.Background(), "A.GCO", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("chunks must respect the reported buffer: %v", err)
	}
	got, _ := l.sim.File("A.GCO")
	if len(got) != 100 {
		t.Fatalf("got %d bytes", len(got))
	}
}

// note records a trace line; l.mu held.
func (l *link) note(s string) {
	l.trace = append(l.trace, s)
	if len(l.trace) > 40 {
		l.trace = l.trace[1:]
	}
}

func (l *link) dump() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.trace, "\n")
}
