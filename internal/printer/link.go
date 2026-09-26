package printer

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"forge/internal/serial"
)

// Link keeps a Driver connected to the printer: it opens the port when it
// appears, hands the Driver to the Manager, watches it, and starts over with
// a backoff when the link dies (USB unplug, printer power-off, MCU halt).
// It is one goroutine plus one 5s ticker while connected.
type Link struct {
	port    string
	baud    int
	bufsize int
	names   *NameMap
	mgr     *Manager

	// Test hooks and tunables; NewLink sets the production values.
	stat         func(string) error
	dial         func(port string, baud int) (Conn, error)
	handshake    time.Duration
	minBackoff   time.Duration
	maxBackoff   time.Duration
	healthyAfter time.Duration // a connection this long resets the backoff
	tick         time.Duration
	probeAfter   time.Duration // RX silence before an M105 probe
	deadAfter    time.Duration // RX silence before the link counts as dead
}

func NewLink(port string, baud, bufsize int, names *NameMap, mgr *Manager) *Link {
	return &Link{
		port:    port,
		baud:    baud,
		bufsize: bufsize,
		names:   names,
		mgr:     mgr,
		stat: func(p string) error {
			_, err := os.Stat(p)
			return err
		},
		dial: func(p string, baud int) (Conn, error) {
			sp, err := serial.Open(p, baud)
			if err != nil {
				return nil, err
			}
			return sp, nil
		},
		handshake:    5 * time.Second,
		minBackoff:   time.Second,
		maxBackoff:   15 * time.Second,
		healthyAfter: 30 * time.Second,
		tick:         5 * time.Second,
		probeAfter:   10 * time.Second,
		deadAfter:    25 * time.Second,
	}
}

// Run keeps the link up until ctx is cancelled. It blocks.
func (l *Link) Run(ctx context.Context) {
	backoff := l.minBackoff
	lastMsg := "" // log each distinct failure once, not every retry
	logOnce := func(msg string) {
		if msg != lastMsg {
			log.Printf("forge: %s", msg)
			lastMsg = msg
		}
	}
	for ctx.Err() == nil {
		d, err := l.connect()
		if err != nil {
			logOnce(fmt.Sprintf("printer on %s not available: %v (retrying)", l.port, err))
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, l.maxBackoff)
			continue
		}

		l.mgr.Attach(d)
		d.Start()
		lastMsg = ""
		log.Printf("forge: printer connected on %s", l.port)
		start := time.Now()
		reason := l.supervise(ctx, d)
		d.Close()
		l.mgr.Detach(reason)
		log.Printf("forge: printer disconnected: %s", reason)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= l.healthyAfter {
			backoff = l.minBackoff
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, l.maxBackoff)
	}
}

// connect stats the port first, so an absent printer costs one stat per
// backoff period and no open attempts.
func (l *Link) connect() (*Driver, error) {
	if err := l.stat(l.port); err != nil {
		return nil, err
	}
	conn, err := l.dial(l.port, l.baud)
	if err != nil {
		return nil, err
	}
	d := New(conn, l.bufsize)
	d.SetNames(l.names)
	if err := d.Handshake(l.handshake); err != nil {
		d.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return d, nil
}

// supervise waits until d dies, ctx ends, or the printer goes silent. M155
// S2 makes Marlin talk every 2s, so 10s of silence earns an M105 probe and
// 25s means the link is dead; both are far above the box's 0.75s freezes.
func (l *Link) supervise(ctx context.Context, d *Driver) string {
	t := time.NewTicker(l.tick)
	defer t.Stop()
	connected := time.Now()
	probed := false
	for {
		select {
		case <-ctx.Done():
			return "shutting down"
		case <-d.Done():
			if err := d.Err(); err != nil {
				return err.Error()
			}
			return "connection closed"
		case <-t.C:
			last := d.LastRX()
			if last.Before(connected) {
				last = connected
			}
			silent := time.Since(last)
			switch {
			case silent >= l.deadAfter:
				return fmt.Sprintf("no data from printer for %s", silent.Round(time.Second))
			case silent >= l.probeAfter:
				if !probed {
					probed = true
					_ = d.Send("M105")
				}
			default:
				probed = false
			}
		}
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	cur *= 2
	if cur > max {
		cur = max
	}
	return cur
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
