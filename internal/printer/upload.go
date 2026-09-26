package printer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"forge/internal/binprotocol"
	"forge/internal/heatshrink"
)

// ProgressFunc is called periodically during upload/stream with 0..100.
type ProgressFunc func(sentBytes, totalBytes int64)

// UploadToSD transfers localPath to the printer's SD card under an 8.3 name
// derived from longName, trying Marlin's BINARY_FILE_TRANSFER protocol
// first and falling back to ASCII M28/M29 if the binary handshake doesn't
// complete. It returns the short (8.3) name the file now lives under.
//
// The caller (server) is responsible for deleting localPath afterward and
// for issuing M23/M24 if the file should print immediately.
func (d *Driver) UploadToSD(ctx context.Context, localPath, longName string, names *NameMap, progress ProgressFunc) (string, error) {
	short := names.Assign(longName)

	info, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}
	size := info.Size()

	if err := d.uploadBinary(ctx, localPath, short, size, progress); err == nil {
		return short, nil
	} else {
		// Binary sync/transfer failed (protocol mismatch, timeout, nack) --
		// fall back to the always-supported ASCII M28/M29 path.
		if err2 := d.uploadASCII(ctx, localPath, short, size, progress); err2 != nil {
			return "", fmt.Errorf("binary transfer failed (%v), ASCII fallback also failed: %w", err, err2)
		}
	}
	return short, nil
}

// uploadBinary drives internal/binprotocol directly over the raw
// connection. It must not be used concurrently with the normal windowed
// G-code queue (the driver's pump/read loops are paused by the caller via
// takeoverRaw while this runs -- see Server.handleUpload).
func (d *Driver) uploadBinary(ctx context.Context, localPath, short string, size int64, progress ProgressFunc) error {
	client := binprotocol.NewClient(d.WriteRaw())
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

// uploadASCII streams the file with M28 <name> / M29, sending plain text
// lines through the normal windowed queue so ADVANCED_OK keeps backpressure
// working exactly as it does for a live print.
func (d *Driver) uploadASCII(ctx context.Context, localPath, short string, size int64, progress ProgressFunc) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	d.Send(fmt.Sprintf("M28 %s", short))

	var sent int64
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			d.Send("M29")
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		clean := stripComment(line)
		sent += int64(len(line)) + 1
		if clean == "" {
			continue
		}
		d.Send(clean)
		if progress != nil {
			progress(sent, size)
		}
	}
	d.Send("M29")
	if progress != nil {
		progress(size, size)
	}
	return scanner.Err()
}

// stripComment removes ';' line comments and "(...)" comments, and trims
// whitespace, matching what Marlin itself ignores -- forge does this for
// stream mode so only meaningful commands occupy the send window.
func stripComment(line string) string {
	if i := strings.IndexByte(line, ';'); i >= 0 {
		line = line[:i]
	}
	for {
		i := strings.IndexByte(line, '(')
		if i < 0 {
			break
		}
		j := strings.IndexByte(line[i:], ')')
		if j < 0 {
			line = line[:i]
			break
		}
		line = line[:i] + line[i+j+1:]
	}
	return strings.TrimSpace(line)
}
