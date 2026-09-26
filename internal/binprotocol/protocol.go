// Package binprotocol implements a client for Marlin 2.1's BINARY_FILE_TRANSFER
// protocol (src/feature/binary_stream.h), used to push a file to the printer's
// SD card compressed with heatshrink instead of ASCII M28/M29.
//
// PROTOCOL UNCERTAINTY (read this before trusting binary transfer on real
// hardware): this client's packet framing (type byte + big-endian uint16
// length + payload + CRC16/CCITT-FALSE trailer, and the OPEN/QUERY/WRITE/
// CLOSE/ABORT packet types) is reconstructed from public documentation of
// Marlin's binary_stream feature and the community "marlin-binary-protocol"
// Python client, NOT from reading Marlin's actual C++ source (the source
// tree was not available in this workspace -- only Configuration.h /
// Configuration_adv.h were present). It has only been verified against the
// fake-Marlin emulator in this package's own tests, which implements the
// *same* assumed framing -- so the round trip proves internal consistency,
// not byte-for-byte compatibility with real firmware.
//
// Because of that, Sync() is required before any transfer, has a short
// timeout, and its failure is meant to be treated by callers as "fall back
// to ASCII M28/M29" (see internal/printer). Verify against real hardware
// before relying on binary transfer in production.
package binprotocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Packet types, per protocol.py / marlin-binary-protocol conventions.
type PacketType uint8

const (
	PacketQuery   PacketType = 0 // host->printer: are you there? printer->host: capabilities
	PacketOpen    PacketType = 1 // host->printer: begin file (name, size, compression)
	PacketWrite   PacketType = 2 // host->printer: a chunk of (possibly compressed) file data
	PacketClose   PacketType = 3 // host->printer: end of file
	PacketAbort   PacketType = 4 // host->printer: cancel transfer
	PacketAck     PacketType = 5 // printer->host: ok, packet accepted
	PacketNack    PacketType = 6 // printer->host: error, resend or abort
)

const (
	syncByte      = 0xFA
	maxPayload    = 4096
	defaultWindow = 512 // bytes-in-flight before waiting for an ack, conservative default
)

// Capabilities describes what the printer reported in response to Query().
type Capabilities struct {
	ProtocolVersion uint8
	MaxWindow       uint16 // printer's receive buffer size for WRITE payloads
	SupportsHS      bool   // printer accepts heatshrink-compressed payloads
}

// Client drives one binary-stream file transfer over a serial connection.
type Client struct {
	rw      io.ReadWriter
	Timeout time.Duration
}

func NewClient(rw io.ReadWriter) *Client {
	return &Client{rw: rw, Timeout: 3 * time.Second}
}

func crc16(data []byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func encodePacket(t PacketType, payload []byte) []byte {
	buf := make([]byte, 0, 4+len(payload)+2)
	buf = append(buf, syncByte, byte(t))
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(payload)))
	buf = append(buf, lenBuf[:]...)
	buf = append(buf, payload...)
	crc := crc16(buf[1:]) // CRC covers type+len+payload, not the sync byte
	var crcBuf [2]byte
	binary.BigEndian.PutUint16(crcBuf[:], crc)
	buf = append(buf, crcBuf[:]...)
	return buf
}

type deadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// readPacket reads one framed packet from r, resyncing on the sync byte.
// If r supports SetReadDeadline (as net.Conn and most serial port wrappers
// do), it is used so a silent peer can't block the read forever; otherwise
// the deadline is only checked between reads (best-effort).
func readPacket(r io.Reader, deadline time.Time) (PacketType, []byte, error) {
	if ds, ok := r.(deadlineSetter); ok {
		_ = ds.SetReadDeadline(deadline)
	}
	hdr := make([]byte, 1)
	for {
		if time.Now().After(deadline) {
			return 0, nil, errors.New("binprotocol: timeout waiting for sync byte")
		}
		n, err := r.Read(hdr)
		if err != nil {
			return 0, nil, err
		}
		if n == 1 && hdr[0] == syncByte {
			break
		}
	}
	rest := make([]byte, 3)
	if err := readFull(r, rest); err != nil {
		return 0, nil, err
	}
	t := PacketType(rest[0])
	length := binary.BigEndian.Uint16(rest[1:3])
	if length > maxPayload {
		return 0, nil, fmt.Errorf("binprotocol: payload too large: %d", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if err := readFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	crcBuf := make([]byte, 2)
	if err := readFull(r, crcBuf); err != nil {
		return 0, nil, err
	}
	check := make([]byte, 0, 3+len(payload))
	check = append(check, byte(t))
	check = append(check, rest[1:3]...)
	check = append(check, payload...)
	want := binary.BigEndian.Uint16(crcBuf)
	if crc16(check) != want {
		return 0, nil, errors.New("binprotocol: CRC mismatch")
	}
	return t, payload, nil
}

func readFull(r io.Reader, buf []byte) error {
	got := 0
	for got < len(buf) {
		n, err := r.Read(buf[got:])
		got += n
		if err != nil {
			if got == len(buf) {
				return nil
			}
			return err
		}
	}
	return nil
}

// Sync sends a Query packet and waits for the printer's capability
// response. Callers should fall back to ASCII M28/M29 if this errors.
func (c *Client) Sync() (Capabilities, error) {
	if _, err := c.rw.Write(encodePacket(PacketQuery, nil)); err != nil {
		return Capabilities{}, err
	}
	t, payload, err := readPacket(c.rw, time.Now().Add(c.Timeout))
	if err != nil {
		return Capabilities{}, err
	}
	if t != PacketAck || len(payload) < 4 {
		return Capabilities{}, fmt.Errorf("binprotocol: unexpected query response type=%d len=%d", t, len(payload))
	}
	caps := Capabilities{
		ProtocolVersion: payload[0],
		MaxWindow:       binary.BigEndian.Uint16(payload[1:3]),
		SupportsHS:      payload[3] != 0,
	}
	return caps, nil
}

// Open begins a transfer for the given 8.3 filename, announcing the
// uncompressed size and whether payloads are heatshrink-compressed.
func (c *Client) Open(name string, uncompressedSize uint32, compressed bool) error {
	payload := make([]byte, 0, 1+4+len(name))
	if compressed {
		payload = append(payload, 1)
	} else {
		payload = append(payload, 0)
	}
	var sizeBuf [4]byte
	binary.BigEndian.PutUint32(sizeBuf[:], uncompressedSize)
	payload = append(payload, sizeBuf[:]...)
	payload = append(payload, []byte(name)...)
	return c.sendAndExpectAck(PacketOpen, payload)
}

// Write sends one chunk of (possibly compressed) file data and waits for
// the ack before returning, keeping the printer's receive window from
// overflowing.
func (c *Client) Write(chunk []byte) error {
	return c.sendAndExpectAck(PacketWrite, chunk)
}

// Close finalizes the transfer.
func (c *Client) Close() error {
	return c.sendAndExpectAck(PacketClose, nil)
}

// Abort cancels a transfer in progress.
func (c *Client) Abort() error {
	if _, err := c.rw.Write(encodePacket(PacketAbort, nil)); err != nil {
		return err
	}
	return nil
}

func (c *Client) sendAndExpectAck(t PacketType, payload []byte) error {
	if _, err := c.rw.Write(encodePacket(t, payload)); err != nil {
		return err
	}
	rt, rp, err := readPacket(c.rw, time.Now().Add(c.Timeout))
	if err != nil {
		return err
	}
	if rt == PacketNack {
		return fmt.Errorf("binprotocol: printer nacked packet type=%d: %q", t, rp)
	}
	if rt != PacketAck {
		return fmt.Errorf("binprotocol: unexpected response type=%d to packet type=%d", rt, t)
	}
	return nil
}

// MaxChunk returns a safe chunk size for Write given negotiated caps.
func MaxChunk(caps Capabilities) int {
	if caps.MaxWindow == 0 || caps.MaxWindow > maxPayload {
		return defaultWindow
	}
	return int(caps.MaxWindow)
}
