# Developing Gonk'd

Notes for hacking on Gonk'd: the hardware rules you must not break, the build
and deploy loop, protocol details, and troubleshooting.

Placeholders used below: `<box-ip>` is your Wi-Fi box's address on your LAN.

## Hard constraints (read before coding)

- **Host box:** Creality Wi-Fi Box WB-01, MediaTek MT7628 580MHz MIPS32
  little-endian, **no FPU**, 128MB RAM (~58MB used at idle), OpenWrt 24.10.4
  (kernel 6.6, musl).
- **Static binary only:** `CGO_ENABLED=0`, stdlib + `golang.org/x/sys`. No
  cgo, no heavy dependencies.
- **Flash is the budget.** The box has no SD card (the microSD lives in the
  printer). Root is jffs2, ~7.9MB total, ~4.8MB free after install. The binary
  is ~7.1MB raw (~2.6MB compressed in jffs2). Watch every new dependency.
- **Flash wear:** only small writes, only on change (`names.json`). Staged
  uploads go to `/tmp` (tmpfs, ~60MB).
- **Memory:** target under ~15MB RSS; GC percent is lowered.
- **Freezes:** the box has unpredictable whole-OS freezes of 0.4 to 0.75s
  (root cause unknown, no kernel log entries, no paging, happens at low CPU).
  Timing-critical work must stay on the printer MCU:
  - SD mode is freeze-proof once printing (freezes only slow the upload).
  - Stream mode survives freezes only via Marlin's buffers (64 planner
    blocks + 16 queued commands + 2048-byte RX). Fine on long moves; may
    pause and leave a blob on dense short segments.
  - Never add host-side realtime requirements or heavy polling.
- **Services on the reference box:** Gonk'd on port 80, LuCI (uhttpd) moved
  to port 81 as a rescue UI, Dropbear SSH on 22 (**RSA keys only**, no
  ed25519; `scp` needs `-O` for the legacy protocol).
- **Printer:** Ender 3 V1, Creality 4.2.2 (STM32F103RET6), CH340 USB at
  `/dev/ttyUSB0`, custom Marlin 2.1.2.7 (see `firmware/`). Marlin creates 8.3
  filenames only; Gonk'd keeps a long-to-short map in `/etc/gonkd/names.json`.
- **Testing etiquette:** do not poll the box (ssh loops, log tailing, API
  polling) during real print tests. Polling perturbs timing on a box that
  already freezes. Pull logs once, afterwards.

## Code layout

```
cmd/gonkd/            main(): flags, wiring, HTTP server
internal/gcode/       line framing, XOR checksums, ok/ADVANCED_OK/resend parsing, send window
internal/heatshrink/  pure-Go heatshrink encoder/decoder
internal/binprotocol/ Marlin binary file transfer client (currently disabled, see below)
internal/serial/      termios2/BOTHER raw mode for 250000 baud, HUPCL cleared
internal/printer/     Driver (serial <-> gcode), link supervisor (reconnect),
                      Manager (job state), SD upload, 8.3 name map, parsers
internal/api/         OctoPrint subset + /gonkd/* JSON routes
internal/web/         embedded single-page UI (go:embed)
files/gonkd.init      procd init script
firmware/             Marlin config used on the reference printer (GPL-3.0)
```

## Toolchain, build, test

Any machine with Go 1.26+ works (Linux, macOS, WSL). If you build from a WSL
checkout on a Windows drive and Go complains about VCS stamping, set
`GOFLAGS=-buildvcs=false`.

```sh
go vet ./...
go test ./...                    # add -race -count=3 before sending a PR
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
  go build -trimpath -ldflags "-s -w" -o gonkd.mipsle ./cmd/gonkd
```

Output is a ~7.1MB static `ELF 32-bit LSB executable, MIPS, MIPS32`.

Tests are all in-process: unit tests per package plus an integration test
that drives the Driver against a fake Marlin over `net.Pipe`. No hardware
needed.

## Deploy loop

Use an SSH config entry for convenience, for example:

```
# ~/.ssh/config
Host printbox
    HostName <box-ip>
    User root
    IdentityFile ~/.ssh/<your-rsa-key>
```

Then:

```sh
scp -O gonkd.mipsle printbox:/tmp/gonkd.new
ssh printbox 'chmod 755 /tmp/gonkd.new && mv /tmp/gonkd.new /usr/bin/gonkd && /etc/init.d/gonkd restart'
```

Copying to `/tmp` first and then `mv` avoids a half-written binary in flash if
the transfer drops. Updating the init script works the same way
(`files/gonkd.init` -> `/etc/init.d/gonkd`).

Restarting Gonk'd during an SD print is safe: the serial port is opened with
`HUPCL` cleared, so DTR does not toggle and the printer is not reset.

### Cheat sheet

| Task | Command |
|---|---|
| Logs | `ssh printbox logread -e gonkd` |
| Restart | `ssh printbox /etc/init.d/gonkd restart` |
| Running? | `ssh printbox 'pgrep -l gonkd'` |
| API check | `curl http://<box-ip>/api/version` |
| Flash usage | `ssh printbox 'df -h /etc/gonkd'` |
| Staging space | `ssh printbox 'df -h /tmp'` |
| Raw printer command | web UI console at `http://<box-ip>/` |

## Protocol notes

- **Framing:** `N<n> <cmd>*<cs>` with the standard Marlin XOR checksum over
  everything before `*` (`internal/gcode/line.go`). Comments are cut at `;`
  and any line whose framing exceeds Marlin's 96-byte command buffer is
  rejected instead of sent (otherwise it livelocks on resends).
- **Flow control:** up to `-bufsize` (Marlin `BUFSIZE`, 16) lines in flight.
  `ADVANCED_OK` replies (`ok N<n> P<planner> B<buffer>`) are parsed.
  `Resend: <n>` replays from that line; a resend for a line no longer
  outstanding (printer reset) triggers a resync. An ack stall triggers a
  replay after a few seconds.
- **Emergency:** `M112`, `M108`, `M410`, `M876` are written raw, bypassing
  the queue (Marlin's `EMERGENCY_PARSER` handles them immediately).
- **Link supervisor:** the HTTP server starts without a printer. The link
  reconnects with backoff and a silence watchdog; on reconnect it handshakes
  (`M110`, `M115` capabilities) before accepting commands. procd respawns
  without a retry cap.
- **Status without polling:** `M155` temperature and `M27 S` SD status
  autoreports drive the UI. No periodic `M20 L`.
- **SD upload (ASCII):** the upload takes the port exclusively, sends
  `M28 <8.3 name>`, waits for `Writing to file`, streams framed lines, then
  `M29` and waits for its ack. Cancel sends `M29` + `M30`. Names already on
  the card are avoided. `print=true` then issues `M23`/`M24`.
- **Job control:** SD pause/resume `M25`/`M24`; SD cancel `M524`, then heaters
  and fan off and park once the abort is confirmed. Stream cancel stops
  sending, then the same cooldown. Pause/cancel act on the running job's mode.
- **Binary transfer (disabled):** the current `internal/binprotocol` and
  `internal/heatshrink` code was written without access to Marlin's source
  and does not match the real protocol. A comparison against Marlin 2.1.2.7's
  `src/feature/binary_stream.h` and `MarlinBinaryProtocol.py` shows the real
  one needs `M28 B1` to enter binary mode, sync bytes `0xB5AD`, Fletcher-16
  checksums, 96-byte payloads, and ASCII-framed replies. Rework pending; the
  path is gated off until then.
- **Babystep:** `M290 Z<d>` mid-print works (`BABYSTEP_ALWAYS_AVAILABLE`).
  With mesh leveling and no probe, `M500` does **not** persist it; the
  persistent equivalent is `G29 S4 Z<offset>` then `M500`.
- **File list:** `M20 L` output is parsed as `<8.3 name> <size> <long name>`
  (long name unquoted); only 8.3 names count as files.

## Troubleshooting

**Printer shows offline.** Check `ls /dev/ttyUSB*` and `dmesg | tail` on the
box. Is `kmod-usb-serial-ch341` installed? Is the printer powered? The link
retries on its own; `logread -e gonkd` shows each attempt.

**Upload slow or stalls.** ASCII upload runs ~9KB/s. Check `/tmp` free space
and the `-max-upload-mb` cap. Bursts of `Resend` / `No Checksum with line
number` in the log are a known issue; the stall replay recovers after ~5s.

**OrcaSlicer "Test" fails.** `curl http://<box-ip>/api/version` should return
JSON with an OctoPrint-style `text` field. If you use a hostname instead of
the IP, add it to `-allow-host`.

**UI buttons return 421.** You are using a hostname that is not in
`-allow-host`.

**File list empty or odd names.** Run `M20 L` in the web console and compare
with the parser in `internal/printer/parse.go`. Name map:
`/etc/gonkd/names.json`.

**Web UI not responding.** On the box: `pgrep -l gonkd`, `logread -e gonkd`,
`curl http://127.0.0.1/api/version`. If the API answers, check the browser
console.

**Binary grew too large.** Inspect with `go version -m gonkd.mipsle` and
`go tool nm -size -sort size` on an unstripped build.

**Print fails with a blob or pause in stream mode.** That is a box freeze
outrunning Marlin's buffers. Use SD mode, or enable arc fitting in the
slicer.
