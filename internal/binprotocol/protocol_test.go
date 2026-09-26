package binprotocol

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/Xoltox/gonkd/internal/heatshrink"
)

// fakeMarlin runs a minimal in-process server implementing this package's
// assumed wire format, so tests can prove packet framing and CRC round trip
// end to end (including reconstructing an uploaded file from WRITE chunks).
// It does NOT prove compatibility with real Marlin firmware -- see the
// package doc comment for why that remains unverified.
func fakeMarlin(t *testing.T, conn net.Conn, received *bytes.Buffer, wantCompressed *bool) {
	t.Helper()
	for {
		typ, payload, err := readPacket(conn, time.Now().Add(2*time.Second))
		if err != nil {
			return
		}
		switch typ {
		case PacketQuery:
			resp := []byte{1, 0x02, 0x00, 1} // version 1, window 512, supports HS
			conn.Write(encodePacket(PacketAck, resp))
		case PacketOpen:
			if len(payload) >= 1 {
				*wantCompressed = payload[0] != 0
			}
			conn.Write(encodePacket(PacketAck, nil))
		case PacketWrite:
			received.Write(payload)
			conn.Write(encodePacket(PacketAck, nil))
		case PacketClose:
			conn.Write(encodePacket(PacketAck, nil))
			return
		case PacketAbort:
			return
		}
	}
}

func TestClientTransferRoundTrip(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	received := &bytes.Buffer{}
	var wasCompressed bool
	done := make(chan struct{})
	go func() {
		fakeMarlin(t, serverConn, received, &wasCompressed)
		close(done)
	}()

	client := NewClient(clientConn)
	client.Timeout = 2 * time.Second

	caps, err := client.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if caps.ProtocolVersion != 1 || caps.MaxWindow != 512 || !caps.SupportsHS {
		t.Fatalf("caps = %+v", caps)
	}

	original := bytes.Repeat([]byte("G1 X10.000 Y10.000 E0.03210 F1500\n"), 300)
	compressed := heatshrink.Encode(original, heatshrink.DefaultConfig())

	if err := client.Open("PRINT~1.GCO", uint32(len(original)), true); err != nil {
		t.Fatalf("Open: %v", err)
	}

	chunkSize := MaxChunk(caps)
	for i := 0; i < len(compressed); i += chunkSize {
		end := i + chunkSize
		if end > len(compressed) {
			end = len(compressed)
		}
		if err := client.Write(compressed[i:end]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done

	if !wasCompressed {
		t.Fatal("server did not see compressed flag set")
	}

	decoded := heatshrink.Decode(received.Bytes(), heatshrink.DefaultConfig(), len(original))
	if !bytes.Equal(decoded, original) {
		t.Fatalf("reconstructed file does not match original: got %d bytes, want %d", len(decoded), len(original))
	}
}

func TestSyncTimesOutWithNoResponder(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go func() {
		// Server never responds; just drain reads until closed.
		buf := make([]byte, 64)
		for {
			if _, err := serverConn.Read(buf); err != nil {
				return
			}
		}
	}()
	client := NewClient(clientConn)
	client.Timeout = 100 * time.Millisecond
	if _, err := client.Sync(); err == nil {
		t.Fatal("expected Sync to time out and error")
	}
	clientConn.Close()
	serverConn.Close()
}

func TestCRCDetectsCorruption(t *testing.T) {
	pkt := encodePacket(PacketOpen, []byte("hello"))
	pkt[len(pkt)-1] ^= 0xFF // corrupt CRC byte
	_, _, err := readPacket(bytes.NewReader(pkt), time.Now().Add(time.Second))
	if err == nil {
		t.Fatal("expected CRC mismatch error")
	}
}
