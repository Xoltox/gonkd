# Project-Beskar Handoff

## What This Is

**forge** (name to change later): lean Go print server on a Creality Wi-Fi Box (WB-01, OpenWrt 24.10.4) driving an Ender 3 running custom Marlin 2.1.2.7 over USB.

- OctoPrint-compatible API subset (POST /api/files/local, M28/M29/M23/M24) so OrcaSlicer "Octo/Klipper" host type can test connection and do one-click upload+print.
- **Upload modes**: POST /api/files/local accepts multipart file + print + select fields. Default: SD mode (stage file in /tmp, heatshrink-compress, send via Marlin BINARY_FILE_TRANSFER packets to printer SD, then M23/M24 if print=true). If binary sync fails, auto-fallback to ASCII M28/M29 (much slower; real speeds not yet measured). Optional stream mode (`-default-mode stream` or per-request choice): line-by-line streaming, no SD copy, freeze-sensitive (see constraints).
- **Web UI** (embedded single page via go:embed, mobile-friendly, polls every 2s, file list every 10s): nozzle/bed temps, state, filename, progress, jog X/Y/Z, home, extrude/retract, preheat PLA/PETG, cooldown, fan on/off (M106/M107), Z babystep +/-0.01/+/-0.05 via M290 with running total and Save (M500), pause/resume/cancel, E-stop M112 (confirm dialog), SD file list with print/delete, console (send commands, last ~100 lines).
- **Testing**: go test ./... passes. Covered: gcode line framing/XOR checksum/ADVANCED_OK (N P B) parsing/Resend recovery, windowed send queue with replay, emergency commands (M112/M108/M410/M876) bypass, heatshrink encoder/decoder round-trip (empty/short/repetitive/random/all-bytes), binary-transfer packet framing with CRC16/CCITT, sync timeout, 8.3 name collision suffix and persistence, temperature/SD-status/capability/file-list line parsing, end-to-end Driver handshake/send/resend/ack/SD upload against in-process fake Marlin via net.Pipe integration_test.go. NOT covered: HTTP API handlers, embedded web UI (glue layer over tested pieces, straightforward, but no httptest).

## HARD CONSTRAINTS (read before coding)

- **Host box**: Creality Wi-Fi Box v1 (WB-01), MediaTek MT7628 580MHz MIPS32 little-endian, NO FPU, 128MB RAM (~58MB idle), OpenWrt 24.10.4 (kernel 6.6), musl.
- **Build**: `CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags "-s -w" -o forge ./cmd/forge`. Static binary only; stdlib + golang.org/x/sys; no cgo, no heavy deps.
- **Storage**: box has no SD card (microSD is in printer). Root is jffs2 flash, 7.9MB total; ~4.8MB free after install. Binary 7.08MB raw (~2.6MB in jffs2). **Binary size is a hard budget** -- watch it. Avoid frequent flash writes (wear): only small config/names.json writes, on change. Staged uploads go to /tmp (tmpfs ~60MB) -- upload cap default 40MB.
- **Memory**: target under ~15MB RSS; GC percent lowered.
- **Freezes**: box experiences unpredictable whole-OS freezes of 0.4-0.75s duration (observed under Klipper, independent of load; root cause unknown, no kernel log entries, no paging). This is WHY Klipper was abandoned. Timing-critical work must stay on the printer MCU. **SD-upload mode is freeze-proof once printing** (freezes only slow the upload). **Stream mode survives freezes only via Marlin buffers** (64 planner blocks + 16 queued commands + 2048-byte RX): fine on long moves, may pause and leave a blob on dense short segments. Arc fitting in Orca helps. Never add host-side realtime requirements or heavy polling.
- **Services on box**: LuCI (uhttpd) port 81 (rescue/fallback admin), forge port 80, dropbear SSH 22 (RSA keys only; no ed25519; scp needs -O legacy proto), ttyd terminal on <box-ap-ip>:7681 (box AP only), dnsmasq DHCP/DNS. Box AP SSID "<ssid>" DHCP <box-ap-ip>/24; also Wi-Fi client to home network IP <box-ip>. SSH/scp alias: `ssh printbox` uses ~/.ssh/<ssh-key> key.
- **Printer**: Ender 3 V1, Creality 4.2.2 board, STM32F103RET6, CH340 USB (/dev/ttyUSB0), custom Marlin 2.1.2.7 (<workspace>\KlipperWrt\marlin\BUILD.md).
- **Marlin firmware**: Custom Marlin 2.1.2.7, env STM32F103RE_creality. Source in <workspace>\KlipperWrt\marlin\, built per BUILD.md there. Compiled with: BAUDRATE 250000, ADVANCED_OK (N/P/B in ok), RX_BUFFER_SIZE 2048, BUFSIZE 16, BLOCK_BUFFER_SIZE 64, EMERGENCY_PARSER (M108/M410/M112 bypass queue), HOST_ACTION_COMMANDS, SET_PROGRESS_MANUALLY (M73 for web UI progress), BINARY_FILE_TRANSFER, AUTO_REPORT_TEMPERATURES (M155 periodic), AUTO_REPORT_SD_STATUS (M27 S periodic), EXTENDED_CAPABILITIES_REPORT, LONG_FILENAME_HOST_SUPPORT, BABYSTEPPING (M290 with M500 save), MESH_BED_LEVELING 5x5 (manual, saved, M420 S1 in start G-code), SDSUPPORT with write (SDCARD_READONLY off). Marlin creates 8.3 filenames only; server maintains long->short map in /etc/forge/names.json.
- **OrcaSlicer setup**: physical printer host type "Octo/Klipper", hostname <box-ip> (port 80), API key blank or anything (forge accepts any). Printer G-code flavor Marlin 2, relative E, start G-code with G28 then M420 S1 (no Klipper macros), Z offset 0 (use babystep), arc fitting on.

## Current State (2026-09-26)

- Repo: <workspace>\Project-Beskar, git main, two commits: c9e32d4 (initial) and 000823b (force LF line endings for init script on OpenWrt).
- Not pushed to GitHub yet.
- Deployed to box 2026-09-26: binary at /usr/bin/forge, init script /etc/init.d/forge (procd enabled), config dir /etc/forge/. First real-hardware test (OrcaSlicer upload+print) pending.
- All tests pass: `go test ./...` (unit + integration across gcode, heatshrink, binprotocol, printer driver, names, parsing).
- Binary size on box: 7.08MB raw ELF, ~2.6MB compressed in jffs2. Flash usage after install: ~4.8MB free of 7.9MB total.
- Code is ~3200 lines of Go (internal/ modules + main).
- Printer side verified 2026-09-26: Marlin flashed, splash shows, EEPROM reset + saved, jog/home/PID OK, 5x5 mesh saved, SD cube printed fine (slight elephant foot; fix via babystep). Old Marlin Z offset values unknown (check M503 from web console).

## Code Layout

- cmd/forge/main.go: flag parsing, serial port setup (termios2/BOTHER at 250kbaud), HTTP server startup, wiring.
- internal/gcode/: line framing (N/*/B checksum parsing), ok/ADVANCED_OK, Resend recovery, windowed send queue.
- internal/heatshrink/: pure-Go LZSS encoder/decoder (window 8, lookahead 4 bits), used for binary file transfer.
- internal/binprotocol/: Marlin binary-file-transfer packet client (sync/open/write/close/abort/ack/nack), CRC16/CCITT, timeout.
- internal/serial/: termios2 raw-mode 250000 baud port setup (non-standard rate via BOTHER).
- internal/printer/: Driver (serial<->gcode glue), Manager (job state machine), 8.3 name collision map with persistence, line parsers (temps, SD status, capabilities, file list).
- internal/api/: OctoPrint-compatible HTTP endpoints (/api/version, /api/server, /api/connection, /api/printer [state + temps], /api/job [progress], /api/files/local [M20 list, upload POST, delete]) to satisfy OrcaSlicer's OctoPrint client; forge's own JSON endpoints for web UI (printer state, job control, manual commands).
- internal/web/: embedded single-page UI (go:embed), polling.
- files/forge.init: procd init script with respawn config and flag defaults.

## Known Gaps / Risks (prioritized)

- **No serial reconnect** (CRITICAL): main.go calls log.Fatalf if /dev/ttyUSB0 missing at start; procd respawn config (3600 5 5) gives up after 5 fast failures. Printer power-cycle or USB unplug causes permanent exit. Needs reconnect loop with backoff.
- **Binary transfer protocol uncertain**: Marlin C++ src (src/feature/binary_stream.h) not available during dev; packet framing (sync byte + type + big-endian length + payload + CRC16/CCITT) and heatshrink params (window 8 bits, lookahead 4 bits) reconstructed from public heatshrink docs and community marlin-binary-protocol Python client. Verified only by round-trip against forge's own fake Marlin in tests (proves internal consistency, not byte-for-byte hardware compatibility). ASCII M28/M29 fallback always available. Expect to adjust packet/heatshrink constants against actual compiled firmware if binary doesn't sync.
- **OrcaSlicer OctoPrint client not verified live**: HTTP subset implemented (/api/version, /api/server, /api/connection, /api/printer, /api/job, /api/files/local) covers public API docs and typical slicer client behavior, but not exercised against live OrcaSlicer (no OrcaSlicer install in sandbox). If upload/test fails, capture HTTP requests via proxy to close any gap.
- **No HTTP handler / web UI tests**: API layer is straightforward glue over unit-tested pieces; not covered by httptest. Worth adding if future changes touch the API.
- **No real pty / qemu smoke test in dev**: integration_test.go uses in-process net.Pipe, not real pty via socat. Binary -h works on box; UPX and qemu-mipsel-static smoke runs would be valuable with real tooling.
- **Old duplicate folder**: <workspace>\KlipperWrt\forge\ may still exist (identical copy, safe to delete by user).

## Backlog (user requests)

- **More babystep features in UI**: live Z offset display, reset to zero, finer step options. Details TBD with user.
- **Rename project / binary**: "forge" is placeholder; final name TBD. When ready: change go.mod module path, cmd/forge/ dir, files/forge.init filename, $PROG in init script, install paths (/usr/bin/*, /etc/init.d/*), and all imports (forge/internal/* -> newname/internal/*).
- **Publish to GitHub**: Not yet pushed.
- **Possibly M503 settings view**: read Marlin stored settings (M503) and display in web UI.

## CLI Flags & Config

forge accepts these CLI flags (defaults shown; see files/forge.init):
- `-listen ":80"` - HTTP listen address/port
- `-port "/dev/ttyUSB0"` - serial port path
- `-baud 250000` - serial baud rate (exact match required; 250kbaud is Marlin default)
- `-data-dir "/tmp/forge"` - staging dir for uploads (must be tmpfs or have >40MB free)
- `-names-file "/etc/forge/names.json"` - persistent long->short filename map (jffs2, small writes only)
- `-max-upload-mb 40` - upload size cap in MB
- `-default-mode sd` - upload mode (sd or stream); stream is less freeze-resistant
- `-bufsize 16` - Marlin BUFSIZE (match firmware config; default 16)

## Dev Workflow

**Toolchain**: Go in WSL at ~/go-install/go/bin (add to PATH). Work in <workspace>/Project-Beskar. Use GOFLAGS=-buildvcs=false if needed.

**Build & test**:
```
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build \
  -trimpath -ldflags "-s -w" -o forge ./cmd/forge
```
Produces ~7.0MB static mipsle ELF (binary 7.08MB raw, ~2.6MB in jffs2).

**Deploy** (ask user before deploying or restarting):
```
scp -O forge.mipsle printbox:/tmp/forge.new
ssh printbox 'mv /tmp/forge.new /usr/bin/forge && /etc/init.d/forge restart'
```
SSH alias printbox uses key ~/.ssh/<ssh-key> to <box-ip>.

**Logs**: `ssh printbox logread -e forge` or `logread | grep forge`

**Critical rules for next chat**:
- Ask user before any deploy/restart on box or before any git push.
- Do not poll the box during real printer tests (polling perturbs timing/freezes).
- Do not make frequent small edits to /etc/forge/names.json (jffs2 wear).

## Important Implementation Notes

- **No cgo**: CGO_ENABLED=0, stdlib + golang.org/x/sys (termios2). Keeps binary static and small.
- **Serial**: termios2/BOTHER for non-standard 250000 baud (Linux only). Must match Marlin BAUDRATE.
- **Checksum**: standard Marlin XOR over "N<n> <cmd>", appended as *<cs>. See internal/gcode/line.go.
- **Name map**: names.json written only when a new long->short mapping is created.
- **Upload speed**: not measured on hardware yet. Rough expectation at 250000 baud: binary ~15-25KB/s before compression gain; ASCII M28/M29 slower. Measure on first real uploads.
- **Job control**: SD pause/resume M25/M24; SD cancel M524 then heaters/fan off and park (internal/printer/job.go). Stream cancel stops sending then same cooldown.

## How to Rename (when ready)

Binary/service name "forge" appears in:
- go.mod: module forge (every import: forge/internal/...)
- cmd/forge/: directory name and build -o flag
- files/forge.init: filename and $PROG variable
- Install paths: /usr/bin/forge, /etc/init.d/forge

Go module path is separate; doesn't need to match binary name but requires find-and-replace across all imports if both are renamed.

## Quick Reference / Command Cheat Sheet

| Task | Command |
|------|---------|
| SSH to box | `ssh printbox` (resolves to <box-ip>) |
| View logs | `ssh printbox logread -e forge` |
| Restart service | `ssh printbox /etc/init.d/forge restart` |
| Verify running | `ssh printbox 'pgrep -l forge'` |
| Test API | `curl http://<box-ip>/api/version` |
| Build (local) | `CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags "-s -w" -o forge ./cmd/forge` |
| Deploy | `scp -O forge.mipsle printbox:/tmp/forge.new && ssh printbox 'mv /tmp/forge.new /usr/bin/forge && /etc/init.d/forge restart'` |
| Check jffs2 usage | `ssh printbox 'df -h /etc/forge'` |
| Raw printer command | web UI console at http://<box-ip> |

## Debugging & Troubleshooting

**Binary transfer fails / falls back to ASCII**: binary handshake likely failed due to bad packet framing or CRC mismatch. Check: (1) Marlin BINARY_FILE_TRANSFER is actually enabled in compiled firmware; (2) heatshrink window/lookahead params match Marlin source src/feature/binary_stream.h; (3) baud rate matches exactly (250000 on both sides). Enable debug logging in binprotocol.go if needed.

**Upload hangs or times out**: check /tmp/forge free space (must be >file size + overhead). If stream mode: OS freezes may cause buffering issues (box freeze pauses read, Marlin RX buffer fills, timeout). SD-upload mode has no timeout per se; if stuck, check dmesg for TX CRC errors or packet loss.

**Printer disconnects / goes offline**: check dmesg on box for CH340 driver errors; USB cable might be loose or power-starved. procd respawn will attempt restart (up to 5 fast failures then back off). No auto-reconnect yet (planned).

**File list shows no files or wrong names**: M20 L parsing is sensitive to exact Marlin output format. Check `ssh printbox 'echo "M20 L" | nc localhost 80'` or logread for parse errors. If 8.3 names look corrupted, check /etc/forge/names.json format.

**Web UI not responding**: SSH to box, check `ps aux | grep forge` (process alive?), `logread -e forge` (errors?), `curl http://localhost/api/version` (API OK?). If API OK but web not loading, check browser console for JS errors.

**Binary size grew too large**: binary size is non-negotiable (~7MB raw, ~2.6MB compressed). Each new dep or heavy function hurts. Profile with `go test -bench` and analyze with `go tool nm`. Prefer pure-Go libs; no cgo allowed.

## Related History

- <workspace>\KlipperWrt\my_config\HANDOFF.md (current box/printer config, WB-01 + Ender 3 V1).
- <workspace>\KlipperWrt\my_config\HANDOFF-klipper-archive.md (why Klipper was abandoned: random OS freezes, MCU timing needed).
