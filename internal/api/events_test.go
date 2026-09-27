package api

import (
	"testing"
	"time"
)

// TestHubFanOut: every connected client gets a broadcast event.
func TestHubFanOut(t *testing.T) {
	h := newHub()
	c1, ok := h.add()
	if !ok {
		t.Fatal("add c1")
	}
	c2, ok := h.add()
	if !ok {
		t.Fatal("add c2")
	}
	h.broadcast(sseEvent{event: "status", data: []byte(`{"a":1}`)})

	for _, c := range []*sseClient{c1, c2} {
		select {
		case ev := <-c.ch:
			if ev.event != "status" || string(ev.data) != `{"a":1}` {
				t.Fatalf("event = %+v", ev)
			}
		default:
			t.Fatal("client did not receive the broadcast event")
		}
	}
}

// TestHubDropsOldestOnFullBuffer: a client that never drains has its oldest
// queued event dropped rather than the sender ever blocking.
func TestHubDropsOldestOnFullBuffer(t *testing.T) {
	h := newHub()
	c, ok := h.add()
	if !ok {
		t.Fatal("add")
	}
	// Fill the buffer, then push more: broadcast must never block, and the
	// client must end up seeing the newest events, not the oldest.
	total := sseClientBuf + 3
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			h.broadcast(sseEvent{event: "console", data: []byte{byte(i)}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full client buffer")
	}

	var got []byte
	for {
		select {
		case ev := <-c.ch:
			got = append(got, ev.data[0])
			continue
		default:
		}
		break
	}
	if len(got) != sseClientBuf {
		t.Fatalf("buffered events = %d, want %d (the per-client cap)", len(got), sseClientBuf)
	}
	// The oldest were dropped, so what remains must be the tail end of the
	// sequence (byte i for i in [total-sseClientBuf, total)), in order.
	for i, b := range got {
		want := byte(total - sseClientBuf + i)
		if b != want {
			t.Fatalf("got[%d] = %d, want %d (oldest events must be dropped first): %v", i, b, want, got)
		}
	}
}

// TestHubMaxClients: the (maxSSEClients+1)th client is refused.
func TestHubMaxClients(t *testing.T) {
	h := newHub()
	for i := 0; i < maxSSEClients; i++ {
		if _, ok := h.add(); !ok {
			t.Fatalf("client %d refused before the cap", i)
		}
	}
	if _, ok := h.add(); ok {
		t.Fatal("client beyond maxSSEClients was accepted")
	}
	if h.count() != maxSSEClients {
		t.Fatalf("count = %d, want %d", h.count(), maxSSEClients)
	}
}

// TestHubRemoveFreesASlot: removing a client makes room for a new one.
func TestHubRemoveFreesASlot(t *testing.T) {
	h := newHub()
	var clients []*sseClient
	for i := 0; i < maxSSEClients; i++ {
		c, ok := h.add()
		if !ok {
			t.Fatalf("client %d refused", i)
		}
		clients = append(clients, c)
	}
	h.remove(clients[0])
	if _, ok := h.add(); !ok {
		t.Fatal("expected room after remove")
	}
}

func TestSSEEventFrame(t *testing.T) {
	ev := sseEvent{event: "console", data: []byte(`{"line":"ok"}`)}
	got := string(ev.frame())
	want := "event: console\ndata: {\"line\":\"ok\"}\n\n"
	if got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}
