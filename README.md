# forge

A lean Go print server that turns a Creality Wi-Fi Box (OpenWrt 24.10.4,
MT7628, 580MHz mipsel, no FPU, 128MB RAM, ~7.5MB free flash) into a minimal
ESP3D/OctoPrint-like host for an Ender 3 running custom Marlin 2.1.2.7 over
USB (CH340, `/dev/ttyUSB0`, 250000 baud).

It speaks a small OctoPrint-compatible HTTP subset so OrcaSlicer's
"Octo/Klipper" host type can test the connection and do one-click
upload+print, plus its own JSON API backing a single embedded mobile-friendly
web page (temps, job progress, jog/extrude controls, Z babystepping, SD file
list, live console). Uploads go to the printer's SD card by default (with an
ASCII fallback if Marlin's binary transfer protocol can't be negotiated);
line-by-line streaming is available as an alternate mode.

The binary and systemd/procd service are currently named `forge` (see
"Renaming" below for the one thing to change if that changes later).

## Requirements on the box

- OpenWrt 24.10.4, mipsel (MT7628), USB-serial (`ch341` kernel module) so
  the Ender 3's CH340 shows up as `/dev/ttyUSB0`.
- Enough free space in `/tmp` (tmpfs, ~60MB total) to stage one upload at a
  time, up to the configured `-max-upload-mb` cap (default 40MB).
- `/etc/forge/` writable, for the small long/short filename map
  (`names.json`), written only when it changes (flash is jffs2).

## Building (WSL)

Go toolchain lives in WSL; this repo's module root is the WSL-visible path
`<workspace>/Project-Beskar` when developed from Windows.

```bash
cd <workspace>/Project-Beskar
go build ./...          # sanity build for the host arch
go test ./...            # unit + integration tests (all in-process, no hardware)
go vet ./...

# Cross-compile for the box:
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
  go build -trimpath -ldflags "-s -w" -o forge ./cmd/forge
```

As built during development this produces a static ~7.0MB mipsle ELF
binary (`file forge` reports `ELF 32-bit LSB executable, MIPS, MIPS32
version 1 (SYSV), statically linked, stripped`). UPX and qemu-user-static
were not available in the dev sandbox (would have needed `sudo` interactively)
so a further UPX-compressed size and a qemu-mipsel-static `--help` smoke run
were not captured here -- worth doing once on real tooling before relying on
either.

## Installing

```bash
scp -O forge root@<box-ip>:/usr/bin/forge
scp -O files/forge.init root@<box-ip>:/etc/init.d/forge
ssh root@<box-ip> 'chmod +x /usr/bin/forge /etc/init.d/forge
                         mkdir -p /etc/forge
                         /etc/init.d/forge enable
                         /etc/init.d/forge start'
```

(`scp -O` forces the legacy SCP protocol, which is what most OpenWrt
`dropbear`/`scp` builds still speak.) Flags (all optional, shown with
defaults) are set in `files/forge.init`:

```
forge -listen ":80" -port "/dev/ttyUSB0" -baud 250000 \
      -data-dir "/tmp/forge" -names-file "/etc/forge/names.json" \
      -max-upload-mb 40 -default-mode sd -bufsize 16
```

Check it's alive: `curl http://<box-ip>/api/version`.

## OrcaSlicer setup

1. Printer Settings -> General -> "Klipper" host type is for Moonraker; pick
   **"Octoprint"** as the host type instead (OrcaSlicer's OctoPrint client
   is what forge's `/api/*` subset targets).
2. Host: `http://<box-ip>`. Port 80 (forge's default `-listen`).
3. API Key: leave blank or put anything -- forge accepts any (or no)
   `X-Api-Key`.
4. "Test" should succeed via `GET /api/version`.
5. Upload/print from Orca posts to `POST /api/files/local` with
   `file`/`print`/`select` multipart fields; forge stages the file under
   `-data-dir`, transfers it to the printer's SD card (or streams it, if
   `-default-mode stream` or `?mode=stream` is set), and issues `M23`/`M24`
   when `print=true`.

## Known gaps / protocol uncertainties (read before relying on this)

- **Binary file transfer (heatshrink + Marlin's `BINARY_FILE_TRANSFER`)**:
  the Marlin C++ source (`src/feature/binary_stream.h`) was not available in
  this workspace to read directly -- only `Configuration.h` /
  `Configuration_adv.h` were present. `internal/binprotocol`'s packet
  framing (sync byte + type + big-endian length + payload + CRC16/CCITT,
  with QUERY/OPEN/WRITE/CLOSE/ABORT/ACK/NACK packet types) and
  `internal/heatshrink`'s bit-packed LZSS format (window=8 bits,
  lookahead=4 bits) are reconstructed from public documentation of
  heatshrink and the community `marlin-binary-protocol` Python client, and
  have only been verified by round-tripping against **forge's own** fake
  Marlin server/decoder in tests -- which proves internal consistency, not
  byte-for-byte compatibility with the real firmware. Because of this,
  `Driver.UploadToSD` always tries binary first and **automatically falls
  back to ASCII `M28`/`M29`** if the initial `Sync()` handshake doesn't
  complete, so uploads should still work even if binary transfer doesn't --
  but confirm on real hardware before counting on the faster path, and
  expect to need to adjust the packet/heatshrink constants against the
  actual compiled firmware if it doesn't sync.
- **OrcaSlicer's OctoPrint client**: the subset implemented
  (`/api/version`, `/api/server`, `/api/connection`, `/api/printer`,
  `/api/job`, `/api/files/local`) covers what publicly documented OctoPrint
  API behavior and common slicer OctoPrint-client implementations use for
  connection-test and one-click upload+print, but wasn't verified against a
  live OrcaSlicer session in this pass (no OrcaSlicer install in the dev
  sandbox). If Orca's "Test" or upload fails, capturing its actual HTTP
  requests (e.g. with a proxy) against `internal/api/api.go`'s routes is the
  fastest way to close any gap.
- **Real pty / qemu smoke test**: the integration test in
  `internal/printer/integration_test.go` emulates Marlin's line
  protocol (ADVANCED_OK, Resend, M28/M29) over an in-process `net.Pipe`
  rather than a real pty via `socat`, and `qemu-mipsel-static` wasn't
  available in the dev sandbox to smoke-run the cross-compiled binary. Both
  would be worth doing once against real tooling.
- The M20 SD file list, M27 status and M155 temperature autoreports are
  parsed from plain text (`internal/printer/parse.go`); Marlin's exact
  spacing/quoting for `LONG_FILENAME_HOST_SUPPORT` file listings has varied
  across versions, so double-check `M20 L` output from this specific build
  if the UI's file list looks wrong.

## What's implemented and tested

- G-code line framing + XOR checksum, "ok"/`ADVANCED_OK` (`N`/`P`/`B`)
  parsing, `Resend:` recovery, busy/error/start classification
  (`internal/gcode`, unit tested).
- A windowed send queue that keeps up to `BUFSIZE` (16) lines outstanding
  and replays from the requested line number on `Resend:` (unit tested).
- Emergency commands (`M112`/`M108`/`M410`/`M876`) bypass the queue and are
  written raw immediately (unit tested).
- A pure-Go heatshrink encoder/decoder, round-tripped against itself over
  empty/short/repetitive/random/all-byte-value inputs (unit tested; see the
  protocol-uncertainty note above on matching real Marlin's exact
  configuration).
- The binary-transfer packet client/framing, round-tripped end-to-end
  (Sync -> Open -> Write chunks -> Close) against an in-process fake Marlin,
  including CRC corruption detection and a sync timeout (unit tested).
- 8.3 short-filename generation with collision suffixing and a persisted
  long/short name map that only writes to disk when it actually changes
  (unit tested).
- Temperature/SD-status/capability/file-list line parsing (unit tested).
- End-to-end Driver behavior (handshake, windowed send/ack, resend
  recovery, ASCII SD upload byte capture) against an in-process fake Marlin
  (integration tested, see the pty caveat above).
- Cross-compiles cleanly for `GOOS=linux GOARCH=mipsle GOMIPS=softfloat`.

Not covered by tests: the HTTP API layer itself (`internal/api`) and the
embedded UI -- these are straightforward glue over the tested pieces above,
but weren't exercised with `httptest` in this pass.

## Renaming later

The binary/service name `forge` appears in exactly these places:
- `cmd/forge/` (the `go build ./cmd/forge` package directory + the `-o`
  flag when building)
- `files/forge.init` (the init script's filename and its `$PROG` path)
- systemd/procd install paths (`/usr/bin/forge`, `/etc/init.d/forge`)

The Go module path (`forge`, in `go.mod`, referenced by every internal
import as `forge/internal/...`) is a separate, larger find-and-replace if
it's ever renamed too -- it doesn't need to match the binary name and
wasn't changed here.

## Layout

```
cmd/forge/            main(): flag parsing, wiring, HTTP server startup
internal/gcode/       line framing, checksums, ok/resend parsing, send window
internal/heatshrink/  pure-Go heatshrink encoder (+ decoder for tests)
internal/binprotocol/ Marlin binary-file-transfer packet client
internal/serial/      termios2/BOTHER raw-mode 250000 baud port setup
internal/printer/     Driver (serial<->gcode glue), Manager (job state),
                      8.3 name map, line parsing (temps/SD status/caps)
internal/api/         OctoPrint-compatible subset + forge's own JSON API
internal/web/         embedded single-page UI (go:embed)
files/forge.init      procd init script
```
