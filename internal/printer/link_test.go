package printer

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// jtDialer hands out net.Pipe fakes instead of opening a tty.
type jtDialer struct {
	mu      sync.Mutex
	fakes   []*jtFake
	servers []net.Conn
	setup   func(f *jtFake)
}

func (d *jtDialer) dial(string, int) (Conn, error) {
	server, client := net.Pipe()
	f := newJTFake(server)
	if d.setup != nil {
		d.setup(f)
	}
	go f.run()
	d.mu.Lock()
	d.fakes = append(d.fakes, f)
	d.servers = append(d.servers, server)
	d.mu.Unlock()
	return client, nil
}

func (d *jtDialer) n() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.fakes)
}

func (d *jtDialer) server(i int) net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.servers[i]
}

func (d *jtDialer) fake(i int) *jtFake {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fakes[i]
}

func jtLink(t *testing.T, dl *jtDialer, tweak func(l *Link)) (*Manager, func()) {
	t.Helper()
	names := NewNameMap(filepath.Join(t.TempDir(), "names.json"))
	mgr := NewManager(names)
	l := NewLink("/dev/fake", 250000, 16, names, mgr)
	l.stat = func(string) error { return nil }
	l.dial = dl.dial
	l.handshake = 300 * time.Millisecond
	l.minBackoff = 10 * time.Millisecond
	l.maxBackoff = 40 * time.Millisecond
	l.tick = 20 * time.Millisecond
	if tweak != nil {
		tweak(l)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Link.Run did not return after cancel")
		}
		dl.mu.Lock()
		for _, s := range dl.servers {
			s.Close()
		}
		dl.mu.Unlock()
	}
	return mgr, stop
}

func TestLinkReconnectsAfterConnectionLoss(t *testing.T) {
	dl := &jtDialer{}
	mgr, stop := jtLink(t, dl, nil)
	defer stop()

	jtWait(t, "first connect", mgr.Connected)
	if s := mgr.Snapshot(); s.State != StateIdle {
		t.Fatalf("connected state %s, want idle", s.State)
	}
	dl.server(0).Close() // printer unplugged
	jtWait(t, "detach", func() bool { return !mgr.Connected() || dl.n() >= 2 })
	jtWait(t, "reconnect", func() bool { return dl.n() >= 2 && mgr.Connected() })
	if err := mgr.Send("M105"); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	jtWait(t, "M105 on the new link", func() bool { return dl.fake(1).count("M105") == 1 })
}

func TestLinkWatchdogDropsSilentPrinter(t *testing.T) {
	dl := &jtDialer{}
	mgr, stop := jtLink(t, dl, func(l *Link) {
		l.probeAfter = 100 * time.Millisecond
		l.deadAfter = 300 * time.Millisecond
	})
	defer stop()
	jtWait(t, "first connect", mgr.Connected)
	dl.fake(0).set(func(f *jtFake) { f.mute = true })
	jtWait(t, "reconnect after silence", func() bool { return dl.n() >= 2 })
	if dl.fake(0).count("M105") == 0 {
		t.Fatal("no M105 probe before declaring the link dead")
	}
}

func TestLinkRetriesWhilePortMissing(t *testing.T) {
	dl := &jtDialer{}
	names := NewNameMap(filepath.Join(t.TempDir(), "names.json"))
	mgr := NewManager(names)
	l := NewLink("/dev/fake", 250000, 16, names, mgr)
	var mu sync.Mutex
	stats := 0
	present := false
	l.stat = func(string) error {
		mu.Lock()
		defer mu.Unlock()
		stats++
		if !present {
			return errors.New("no such device")
		}
		return nil
	}
	l.dial = dl.dial
	l.handshake = 300 * time.Millisecond
	l.minBackoff = 5 * time.Millisecond
	l.maxBackoff = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
		for _, s := range dl.servers {
			s.Close()
		}
	}()

	jtWait(t, "a few stat retries", func() bool { mu.Lock(); defer mu.Unlock(); return stats >= 3 })
	if dl.n() != 0 || mgr.Connected() {
		t.Fatal("opened the port although stat failed")
	}
	mu.Lock()
	present = true
	mu.Unlock()
	jtWait(t, "connect once the port appears", mgr.Connected)
}

func TestNextBackoff(t *testing.T) {
	b := time.Second
	var got []time.Duration
	for i := 0; i < 6; i++ {
		b = nextBackoff(b, 15*time.Second)
		got = append(got, b)
	}
	want := []time.Duration{2, 4, 8, 15, 15, 15}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff sequence %v", got)
		}
	}
}
