package printer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Xoltox/gonkd/internal/binprotocol"
	"github.com/Xoltox/gonkd/internal/gcode"
	"github.com/Xoltox/gonkd/internal/heatshrink"
)

// BinaryTransferEnabled gates the binary upload path. Off until it is
// reworked: it never sends "M28 B1" to switch Marlin into binary mode, and
// it reads the port directly while readLoop is also reading it (bytes get
// split between the two, and its read deadline is left set afterwards).
var BinaryTransferEnabled = false

// ProgressFunc is called periodically during upload/stream with 0..100.
type ProgressFunc func(sentBytes, totalBytes int64)

const (
	uploadMaxLine  = 1 << 20          // longest source line accepted (slicer config comments can be long)
	m28Timeout     = 30 * time.Second // M28 must answer "Writing to file" or "open failed" by then
	listingTimeout = 10 * time.Second // fresh M20 listing before picking a short name
	closeTimeout   = 15 * time.Second // M29 after a cancel or failure
)

// drainProbeAfter: quiet this long after M29, probe with M105. A variable
// so tests can shorten it.
var drainProbeAfter = 10 * time.Second

// UploadToSD transfers localPath to the printer's SD card under an 8.3 name
// derived from longName and returns that short name. It holds the port
// exclusively: every other Send and RefreshFiles gets ErrBusy until it
// returns (emergency commands still go through). It returns only after
// every file line and the closing M29 are acked, i.e. the file is closed on
// the card. On ctx cancel it stops, sends M29 (and M30 to delete the
// partial file) and returns ctx.Err(). On any failure the name mapping is
// forgotten.
//
// The caller is responsible for deleting localPath afterward and for
// issuing M23/M24 if the file should print immediately.
func (d *Driver) UploadToSD(ctx context.Context, localPath, longName string, names *NameMap, progress ProgressFunc) (string, error) {
	if names == nil {
		return "", errors.New("upload: no name map")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}
	size := info.Size()

	gen, err := d.beginUpload()
	if err != nil {
		return "", err
	}
	defer d.endUpload()

	// Pick the short name against a fresh listing so a file already on the
	// card (not created by gonkd) is never overwritten by M28.
	if err := d.refreshListing(ctx, gen); err != nil {
		if ctx.Err() != nil || errors.Is(err, errPrinterReset) || errors.Is(err, ErrClosed) {
			return "", err
		}
		log.Printf("gonkd: upload: SD listing not refreshed (%v), using the last one", err)
	}
	short := d.assignShort(names, longName)

	if BinaryTransferEnabled {
		err = d.uploadBinary(ctx, localPath, short, size, progress)
		if err != nil {
			// Binary sync/transfer failed (protocol mismatch, timeout, nack):
			// fall back to the always-supported ASCII M28/M29 path.
			if err2 := d.uploadASCII(ctx, gen, localPath, short, size, progress); err2 != nil {
				err = fmt.Errorf("binary transfer failed (%v), ASCII fallback also failed: %w", err, err2)
			} else {
				err = nil
			}
		}
	} else {
		err = d.uploadASCII(ctx, gen, localPath, short, size, progress)
	}
	if err != nil {
		names.Forget(short)
		return "", err
	}
	return short, nil
}

func (d *Driver) beginUpload() (uint64, error) {
	d.qmu.Lock()
	defer d.qmu.Unlock()
	select {
	case <-d.dead:
		return 0, ErrClosed
	default:
	}
	if d.uploading.Load() {
		return 0, ErrBusy
	}
	d.uploading.Store(true)
	d.writeErr.Store(false)
	return d.gen.Load(), nil
}

func (d *Driver) endUpload() {
	d.qmu.Lock()
	d.uploading.Store(false)
	d.m28.Store(m28Idle)
	d.qmu.Unlock()
}

// sendOwned queues one of the upload's own lines.
func (d *Driver) sendOwned(ctx context.Context, gen uint64, cmd string) error {
	return d.enqueue(ctx, cmd, true, gen, 0)
}

// waitFor blocks until cond is true, re-checking on every driver state
// change (ack, dequeue, listing, reset). It fails on printer reset, driver
// death, ctx, or after timeout (0 = none) with ErrTimeout.
func (d *Driver) waitFor(ctx context.Context, gen uint64, timeout time.Duration, cond func() bool) error {
	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	for {
		wake := d.changed.wait()
		if d.gen.Load() != gen {
			return errPrinterReset
		}
		if cond() {
			return nil
		}
		select {
		case <-wake:
		case <-d.dead:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		case <-expired:
			return ErrTimeout
		}
	}
}

// drained reports that every queued line has been sent and acked.
func (d *Driver) drained() bool {
	return d.queued.Load() == 0 && d.window.Outstanding() == 0
}

func (d *Driver) refreshListing(ctx context.Context, gen uint64) error {
	seq := d.listSeq.Load()
	if err := d.sendOwned(ctx, gen, "M20 L"); err != nil {
		return err
	}
	return d.waitFor(ctx, gen, listingTimeout, func() bool { return d.listSeq.Load() != seq })
}

// assignShort picks the 8.3 name, avoiding both names.json entries and the
// files in the current SD listing.
func (d *Driver) assignShort(names *NameMap, longName string) string {
	onCard := map[string]bool{}
	for _, f := range d.SDFiles() {
		onCard[strings.ToUpper(f.Short)] = true
	}
	return names.AssignAvoiding(longName, func(s string) bool { return onCard[strings.ToUpper(s)] })
}

// uploadASCII streams the file with M28 <name> / M29 through the normal
// windowed queue, so Marlin's acks pace it exactly as they do a live print.
// Marlin writes every line it receives into the open file, which is why
// the upload must own the port.
func (d *Driver) uploadASCII(ctx context.Context, gen uint64, localPath, short string, size int64, progress ProgressFunc) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Open the file and make sure Marlin really did: after a failed M28
	// every following line would run as live G-code.
	d.m28.Store(m28Waiting)
	if err := d.sendOwned(ctx, gen, "M28 "+short); err != nil {
		return err
	}
	if err := d.waitFor(ctx, gen, m28Timeout, func() bool { return d.m28.Load() != m28Waiting }); err != nil {
		d.abortUpload(gen, short)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("M28 %s: %w", short, err)
	}
	if d.m28.Load() != m28Opened {
		return fmt.Errorf("M28 %s: printer could not open the file", short)
	}

	var sent, reported int64
	lineNo := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), uploadMaxLine)
	for scanner.Scan() {
		line := scanner.Text()
		lineNo++
		sent += int64(len(line)) + 1
		cmd := stripComment(line)
		if cmd == "" || isSaveControl(cmd) || hasM110(cmd) {
			continue
		}
		if !d.fits(cmd) {
			// Marlin would truncate it, checksum included, and ask for it
			// again forever with the file open.
			d.abortUpload(gen, short)
			return fmt.Errorf("G-code line %d is too long for the printer: %d bytes after comment removal, framed line would exceed Marlin's %d-byte limit",
				lineNo, len(cmd), gcode.MaxFramedLen)
		}
		if err := d.sendOwned(ctx, gen, cmd); err != nil {
			if ctx.Err() != nil {
				d.abortUpload(gen, short)
				return ctx.Err()
			}
			return err
		}
		if progress != nil && sent-reported >= 4096 {
			reported = sent
			progress(sent, size)
		}
	}
	if err := scanner.Err(); err != nil {
		d.abortUpload(gen, short)
		return fmt.Errorf("read %s: %w", localPath, err)
	}

	if err := d.sendOwned(ctx, gen, "M29"); err != nil {
		if ctx.Err() != nil {
			d.abortUpload(gen, short)
			return ctx.Err()
		}
		return err
	}
	if err := d.waitDrained(ctx, gen); err != nil {
		if ctx.Err() != nil {
			d.abortUpload(gen, short) // M29 is already queued; this deletes the file
			return ctx.Err()
		}
		return err
	}
	if d.writeErr.Load() {
		return fmt.Errorf("printer reported an SD write error for %s", short)
	}
	if progress != nil {
		progress(size, size)
	}
	return nil
}

// waitDrained waits until the M29 (and everything before it) is acked. A
// lost final "ok" would otherwise hang here forever, because Marlin only
// asks for a resend when a later line arrives: after drainProbeAfter of
// silence an M105 is sent. Its "ok T:..." acks that M105 and everything
// before it (gcode.Window.Ack), and if the M29 itself was lost, its later
// line number makes Marlin request the M29 again. (The file is closed by
// then, so M105 never lands in it.)
func (d *Driver) waitDrained(ctx context.Context, gen uint64) error {
	for {
		err := d.waitFor(ctx, gen, drainProbeAfter, d.drained)
		if err != ErrTimeout {
			return err
		}
		log.Printf("gonkd: upload: no ack for the final lines, probing with M105")
		if err := d.sendOwned(ctx, gen, "M105"); err != nil {
			return err
		}
	}
}

// WaitDrained waits until every line queued so far has been sent and acked
// (the send queue is empty and nothing is outstanding in the window),
// probing with M105 if nothing acks for a while so a lost final "ok" cannot
// hang it forever. It returns once ctx is done, the driver dies, or a
// printer reset changes the generation. The stream feeder uses it to know
// every line has truly been printed, not just handed to Send, before
// marking the job complete.
func (d *Driver) WaitDrained(ctx context.Context) error {
	return d.waitDrained(ctx, d.gen.Load())
}

// abortUpload closes a partial upload: M29 so Marlin leaves saving mode,
// then M30 to delete the partial file. Best effort and time-limited; after
// a printer reset (gen changed) there is nothing to close.
func (d *Driver) abortUpload(gen uint64, short string) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if d.sendOwned(ctx, gen, "M29") != nil {
		return
	}
	if d.waitFor(ctx, gen, 0, d.drained) != nil {
		return
	}
	_ = d.sendOwned(ctx, gen, "M30 "+short)
}

// isSaveControl reports M28/M29 lines inside a G-code file: sent during an
// upload they would end the save early (and the rest of the file would run
// live) or nest it, so they are never forwarded.
func isSaveControl(cmd string) bool {
	w := strings.ToUpper(firstField(cmd))
	return w == "M28" || w == "M29"
}

func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// uploadBinary drives internal/binprotocol directly over the raw
// connection. Unused while BinaryTransferEnabled is false; it still reads
// the port concurrently with readLoop, which must be solved (readLoop as
// the only reader) before it can be enabled.
func (d *Driver) uploadBinary(ctx context.Context, localPath, short string, size int64, progress ProgressFunc) error {
	client := binprotocol.NewClient(d.conn)
	client.Timeout = 3 * time.Second

	caps, err := client.Sync()
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	compressed := heatshrink.Encode(data, heatshrink.DefaultConfig())

	if err := client.Open(short, uint32(size), true); err != nil {
		return fmt.Errorf("open: %w", err)
	}

	chunk := binprotocol.MaxChunk(caps)
	sent := 0
	for sent < len(compressed) {
		select {
		case <-ctx.Done():
			_ = client.Abort()
			return ctx.Err()
		default:
		}
		end := sent + chunk
		if end > len(compressed) {
			end = len(compressed)
		}
		if err := client.Write(compressed[sent:end]); err != nil {
			_ = client.Abort()
			return fmt.Errorf("write: %w", err)
		}
		sent = end
		if progress != nil {
			// Report progress in terms of the (larger) uncompressed size so
			// the UI's percentage tracks the actual file, not the wire
			// bytes, using compression ratio as an estimate.
			ratio := float64(sent) / float64(len(compressed))
			progress(int64(ratio*float64(size)), size)
		}
	}
	if err := client.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if progress != nil {
		progress(size, size)
	}
	return nil
}

// stripComment prepares one source line for the wire so that Marlin stores
// exactly the bytes gonkd checksums: a ';' comment is cut off for every
// command, M117/M118 included (Marlin's process_stream_char drops it at
// receive time, and the checksum with it: this build has no
// GCODE_QUOTED_STRINGS), then backslashes (Marlin's escape character,
// dropped on receipt) and control bytes other than tab are removed, and
// whitespace is trimmed. Parentheses are left alone (no PAREN_COMMENTS).
// Note the SD copy of an uploaded file is therefore comment-stripped.
func stripComment(line string) string {
	line = gcode.CutComment(line)
	if !gcode.WireSafe(line) {
		// Byte-wise, so invalid UTF-8 in the file passes through unchanged.
		b := make([]byte, 0, len(line))
		for i := 0; i < len(line); i++ {
			if c := line[i]; c != '\\' && (c >= 0x20 || c == '\t') {
				b = append(b, c)
			}
		}
		line = string(b)
	}
	return strings.TrimSpace(line)
}
