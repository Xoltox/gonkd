<div align="center">

# Gonk'd

**A stubborn little print server for a Wi-Fi box that keeps blacking out.**

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/OpenWrt-mipsle%20softfloat-00B5E2.svg)](#hardware)
[![Firmware](https://img.shields.io/badge/Marlin-2.1.2.7-orange.svg)](firmware/)
[![Deps](https://img.shields.io/badge/deps-stdlib%20%2B%20x%2Fsys-lightgrey.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-works%20on%20my%20Ender-yellow.svg)](#limitations)

*Slice in OrcaSlicer. Hit "Upload & Print". Walk away. Gonk.*

</div>

---

Gonk'd is a single static Go binary that turns a **Creality Wi-Fi Box (WB-01)**
running OpenWrt into a lean, local-only print host for a **Marlin** printer on
USB. It speaks just enough of the OctoPrint API for OrcaSlicer's one-click
**Upload & Print**, and serves a small mobile-friendly web page for the
everyday stuff: temps, progress, jog, preheat, babystep, pause, cancel,
E-stop, SD files and a console.

No cloud. No Python. No plugins. About 7MB on disk, a few MB of RAM, and it
fits in the box's internal flash with room to spare.

The name is a nod to a certain boxy little utility droid from a galaxy far,
far away that waddles around saying its one word and doing its one job. It is
also what the Wi-Fi box does every few minutes: it gets *gonked* (conked out)
for half a second. Gonk'd is built to not care.

## Table of contents

- [The story](#the-story)
- [Features](#features)
- [How it works](#how-it-works)
- [Hardware](#hardware)
- [Quick start](#quick-start)
- [OrcaSlicer setup](#orcaslicer-setup)
- [SD mode vs stream mode](#sd-mode-vs-stream-mode)
- [Configuration](#configuration)
- [HTTP API](#http-api)
- [Security notes](#security-notes)
- [Limitations](#limitations)
- [Roadmap](#roadmap)
- [Development](#development)
- [Credits](#credits)
- [License](#license)

## The story

It started with a Creality Wi-Fi Box on the shelf and an Ender 3 V1 that
deserved wireless printing: truly local (no Creality Cloud), lean, and one
click from OrcaSlicer.

**Attempt one: Klipper on the box.** The box was flashed with OpenWrt via the
KlipperWrt project and ran Klipper, Moonraker and Fluidd. It printed... sometimes.
Then came the `Timer too close` and `Communication timeout during homing`
shutdowns. The LCD rendering was disabled, bed mesh skipped, a custom
low-overhead LCD module written, status polling cut back, heater timing
padded. Still failing. The logs finally told the truth: the **whole OS
freezes for 0.4 to 0.75 seconds** at random, with no kernel log entry and no
paging. Klipper schedules motion and heater events a fraction of a second
ahead, so a host that naps for 0.75s kills the print. Final score: 2 prints
passed, 5+ failed.

**The pivot.** A Raspberry Pi would have fixed it, but they were out of stock
locally, and the box was already there, already on the network, already
running OpenWrt. So the plan flipped: put Marlin back on the printer, where
**all timing lives on the MCU**, and make the box a thin, uncomplaining host.
If the box freezes, the worst case is a brief pause, never a shutdown. The
printer keeps its own SD card and LCD as a fallback.

**Why Go.** The box's only microSD card moved to the printer, so the server
had to fit in the box's ~7.9MB internal flash. Python does not fit. A single
static Go binary does.

The brief was simple: *"Don't want anything fancy, just want to be able to
send a print from Orca, ... plus basic controls."* That is Gonk'd.

## Features

- **OrcaSlicer Upload & Print** via an OctoPrint-compatible API subset
  (connection test, upload, auto-start).
- **SD-card mode (default):** uploads the G-code to the printer's SD card,
  then prints from SD. Once printing starts, the box is irrelevant.
- **Stream mode (optional):** classic line-by-line streaming with
  `ADVANCED_OK` flow control and resend recovery.
- **Web UI** (single embedded page, phone friendly):
  - nozzle/bed temps, state, filename, progress
  - home, jog X/Y/Z, extrude/retract, preheat PLA/PETG, cooldown, fan
  - Z babystepping (`M290`) with running total
  - pause / resume / cancel, emergency stop (`M112`, with confirm)
  - SD file list with print and delete
  - console: send commands, see recent output
- **Resilient serial link:** reconnects with backoff when the printer is
  power-cycled or unplugged; the web server comes up even without a printer.
  Reopening the port does not reset the printer mid-print.
- **Long filenames:** Marlin only writes 8.3 names; Gonk'd keeps a small
  long-to-short name map and only writes it to flash when it changes.
- **Emergency commands** (`M112`, `M108`, `M410`, `M876`) skip the queue.
- **Tiny footprint:** stdlib + `golang.org/x/sys`, no cgo, static binary.

## How it works

```
  +-------------+   HTTP :80    +-----------------------------------+   USB serial    +------------------+
  | OrcaSlicer  | ------------> |  Creality Wi-Fi Box (OpenWrt)     |  250000 baud    |  Ender 3 V1      |
  | (Upload &   |  OctoPrint    |                                   | --------------> |  Marlin 2.1.2.7  |
  |  Print)     |  API subset   |  gonkd                            |  /dev/ttyUSB0   |                  |
  +-------------+               |   api/     OctoPrint subset +     |                 |  - motion and    |
                                |            /gonkd/* JSON          |                 |    heater timing |
  +-------------+   HTTP :80    |   web/     embedded UI            |                 |  - planner (64)  |
  | Phone /     | ------------> |   printer/ job manager, link      |                 |  - SD card       |
  | browser     |   web UI      |            supervisor, SD upload  |                 |  - autoreports   |
  +-------------+               |   gcode/   framing, checksums,    |                 |    (M155, M27 S) |
                                |            ok/resend window       |                 +------------------+
                                |   serial/  termios2 raw 250k baud |
                                |                                   |
                                |  /tmp  (tmpfs) staged uploads     |
                                |  /etc/gonkd  names.json (flash)   |
                                +-----------------------------------+
```

The golden rule: **nothing time-critical depends on the box.** Marlin plans
motion and runs heaters on its own MCU. Gonk'd only moves bytes, reads
autoreports (Marlin pushes temps and SD status on its own, so there is no
heavy polling), and keeps a window of up to `BUFSIZE` lines in flight.

## Hardware

### The freezes (read this first)

The WB-01 suffers unpredictable **whole-OS freezes of 0.4 to 0.75 seconds**.
Root cause unknown: no kernel log entries, no memory pressure, no paging, and
it happens even at ~4% CPU. This is why Klipper does not work reliably on
this box, and it shapes every design decision here:

- Timing lives on the printer's MCU, never on the host.
- SD mode is freeze-proof once printing (a freeze only slows the upload).
- Stream mode rides out freezes on Marlin's buffers (64 planner blocks,
  16 queued commands, 2KB RX). Fine on long moves; on dense short segments a
  freeze can cause a tiny pause and a blob. Arc fitting in the slicer helps.
- No host-side realtime work and no heavy polling, ever.

### Tested

| Part | Details |
|---|---|
| Host | Creality Wi-Fi Box WB-01: MediaTek MT7628, 580MHz MIPS32 LE, **no FPU**, 128MB RAM, ~7.9MB flash (jffs2) |
| OS | OpenWrt 24.10.4 (installed via KlipperWrt), running from internal flash, no SD card in the box |
| Printer | Ender 3 V1, Creality 4.2.2 board, STM32F103RET6, A4988-class drivers, CH340 USB |
| Firmware | Custom Marlin 2.1.2.7, config in [`firmware/`](firmware/) |

### Might work (untested)

- Other Marlin printers with the same host features enabled (see
  [`firmware/BUILD.md`](firmware/BUILD.md)): `BAUDRATE 250000` (or pass
  `-baud`), `ADVANCED_OK`, `EMERGENCY_PARSER`, `AUTO_REPORT_TEMPERATURES`,
  `AUTO_REPORT_SD_STATUS`, `LONG_FILENAME_HOST_SUPPORT`, SD write support.
- Other OpenWrt (or any Linux) devices with USB and a Go-supported CPU.
  Just change `GOARCH`.
- Stock Creality Marlin builds probably lack several of the features above.

### Budgets on the reference box

| Resource | Budget |
|---|---|
| Binary | ~7.1MB raw, ~2.6MB after jffs2 compression |
| Flash free after install | ~4.8MB of 7.9MB |
| RAM | target under ~15MB RSS (idle is a few MB) |
| Upload staging | `/tmp` tmpfs (~60MB), default cap 40MB per file |

## Quick start

You need Go (1.26+) on your dev machine and SSH/root on the box.

**1. Flash the printer** with a Marlin build that has the host features on.
The exact config used here is in [`firmware/`](firmware/).

**2. Prep the box** (OpenWrt with USB serial support):

```sh
ssh root@<box-ip>
opkg update && opkg install kmod-usb-serial-ch341   # CH340 -> /dev/ttyUSB0
# Gonk'd wants port 80; move LuCI (uhttpd) to 81 so you keep a rescue UI:
uci delete uhttpd.main.listen_http
uci add_list uhttpd.main.listen_http='0.0.0.0:81'
uci commit uhttpd && /etc/init.d/uhttpd restart
```

(Or keep LuCI on 80 and run Gonk'd with `-listen ":8080"`.)

**3. Build** (cross-compile for mipsle, softfloat because there is no FPU):

```sh
git clone https://github.com/Xoltox/gonkd.git && cd gonkd
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
  go build -trimpath -ldflags "-s -w" -o gonkd.mipsle ./cmd/gonkd
```

**4. Install:**

```sh
scp -O gonkd.mipsle root@<box-ip>:/usr/bin/gonkd
scp -O files/gonkd.init root@<box-ip>:/etc/init.d/gonkd
ssh root@<box-ip> 'chmod +x /usr/bin/gonkd /etc/init.d/gonkd &&
  mkdir -p /etc/gonkd && /etc/init.d/gonkd enable && /etc/init.d/gonkd start'
```

`scp -O` forces the legacy SCP protocol that OpenWrt's Dropbear speaks.

**5. Check it:**

```sh
curl http://<box-ip>/api/version
```

Then open `http://<box-ip>/` on your phone.

## OrcaSlicer setup

1. Printer settings -> physical printer -> host type **OctoPrint**
   (listed as "Octo/Klipper" in some versions).
2. Hostname: `http://<box-ip>` (port 80 unless you changed `-listen`).
3. API key: leave blank or type anything. Gonk'd ignores it.
4. Click **Test**. Gonk'd reports an OctoPrint-style version string, which
   Orca requires before it accepts the host.
5. Slice, then **Upload & Print**.

Recommended printer profile bits for this setup: G-code flavor Marlin 2,
relative E, start G-code with `G28` then `M420 S1` (enable the saved mesh),
Z offset 0 in the slicer (tune with babystep instead), **arc fitting on**.

## SD mode vs stream mode

| | SD mode (default) | Stream mode |
|---|---|---|
| How | Upload to printer SD (`M28`/`M29`), then `M23`/`M24` | Send G-code line by line during the print |
| Box freeze during print | Irrelevant | Absorbed by Marlin buffers; possible tiny pause on dense segments |
| Box reboot during print | Print continues | Print stops |
| Start delay | Upload time first (ASCII today: ~9KB/s, a 3.3MB file takes ~6 min) | Starts immediately |
| Printer LCD / SD fallback | File stays on the card | Nothing on the card |

Pick per server with `-default-mode sd|stream`, or per upload with
`?mode=stream`. The default is SD, on purpose.

## Configuration

Flags are set in `/etc/init.d/gonkd`. Defaults:

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:80` | HTTP listen address |
| `-port` | `/dev/ttyUSB0` | serial device |
| `-baud` | `250000` | must match Marlin `BAUDRATE` |
| `-data-dir` | `/tmp/gonkd` | upload staging (keep it on tmpfs) |
| `-names-file` | `/etc/gonkd/names.json` | long/short filename map (tiny, written only on change) |
| `-max-upload-mb` | `40` | reject bigger uploads |
| `-default-mode` | `sd` | `sd` or `stream` |
| `-bufsize` | `16` | lines in flight; match Marlin `BUFSIZE` |
| `-allow-host` | (empty) | extra `Host` names to accept, comma separated (IPs and localhost always work) |

If you reach the UI by hostname (e.g. `http://printbox.lan`), add that name to
`-allow-host`, or the buttons get HTTP 421.

## HTTP API

OctoPrint subset (what OrcaSlicer uses):

| Route | Purpose |
|---|---|
| `GET /api/version`, `GET /api/server` | connection test |
| `GET/POST /api/connection` | connection state |
| `GET /api/printer` | state and temps |
| `GET/POST /api/job` | progress, job commands |
| `GET/POST /api/files/local` | SD file list, multipart upload (`file`, `print`, `select`) |

Gonk'd's own JSON routes for the UI live under `/gonkd/*`: `status`, `send`,
`console`, `files`, `files/delete`, `job/pause`, `job/resume`, `job/cancel`,
`job/print`, `emergency`, `babystep`, `jog`.

## Security notes

Gonk'd is a LAN appliance with **no authentication**. Anyone who can reach
port 80 can heat the hotend and move the axes.

- Keep it on a trusted network. Do **not** port-forward it to the internet.
  Use a VPN if you need remote access.
- The OctoPrint API key is accepted but ignored.
- Mutating routes are POST-only, check `Origin` and `Host`, and require a JSON
  content type (multipart for uploads), which blocks drive-by requests from
  other web pages in your browser (CSRF / DNS rebinding).
- Uploads stream to disk with a size cap and a free-space check; file names
  in the UI are rendered as text, not HTML.
- The HTTP server sets header and idle timeouts and a header size cap. It is
  still a tiny box: it is not built to survive a determined flood.
- Keep SSH on keys only. (Note: the WB-01's Dropbear build is RSA-only, no
  ed25519.)

## Limitations

Honest list. This is a hobby project running on one printer.

- **Binary file transfer is off.** Uploads use ASCII `M28`/`M29` (~9KB/s).
  Marlin's binary protocol with heatshrink is on the roadmap.
- **Occasional resend storms during uploads.** On real hardware some upload
  lines get rejected and recovery currently waits on a ~5s stall replay.
  Uploads complete, just slower. Being fixed.
- **Babystep "Save" does not persist** on a mesh-leveling, probe-less config:
  `M500` does not store `M290` offsets. Use `G29 S4 Z<offset>` + `M500`.
- Stream mode and very dense G-code on this box: see the freeze section.
- One printer per instance. No webcam, no timelapse, no accounts, no plugins.
- Tested on exactly one box, one printer, one firmware build.

## Roadmap

- [ ] Real Marlin binary file transfer (`M28 B1`, heatshrink) for much
      faster uploads
- [ ] Fix the resend path under load
- [ ] Babystep: live Z display, reset, persistent save via `G29 S4`
- [ ] Read-only `M503` settings view in the UI
- [ ] Long file names on the printer's own LCD (Marlin long filename write)
- [ ] UPX and emulator smoke tests for the mipsle build
- [x] Serial reconnect with backoff
- [x] Survive printer resets and restarts without killing an SD print
- [x] OrcaSlicer Upload & Print, verified on hardware

## Development

```sh
go vet ./...
go test ./...        # unit + integration against an in-process fake Marlin
```

Layout, protocol notes, deploy loop and troubleshooting are in
[`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md). Firmware notes are in
[`firmware/BUILD.md`](firmware/BUILD.md).

Bug reports and pull requests are welcome. It is a side project, so replies
may take a bit.

## Credits

- [KlipperWrt](https://github.com/ihrapsa/KlipperWrt) - made OpenWrt on this
  box possible in the first place.
- [OpenWrt](https://openwrt.org/)
- [Marlin](https://marlinfw.org/) and the MarlinFirmware Configurations repo
- [Klipper](https://www.klipper3d.org/), Moonraker and Fluidd - the first
  attempt, and a great teacher about host timing
- [heatshrink](https://github.com/atomicobject/heatshrink)
- [OrcaSlicer](https://github.com/SoftFever/OrcaSlicer)
- [Go](https://go.dev/) and `golang.org/x/sys`

Built with AI assistance (Claude, via Claude Code).

Gonk'd is an independent project. It is not affiliated with or endorsed by
Creality, OctoPrint, Marlin, OrcaSlicer, Lucasfilm or Disney.

## License

[MIT](LICENSE) (c) 2026 Parth Sharma.

The Marlin configuration files in [`firmware/`](firmware/) are GPL-3.0, see
[`firmware/LICENSE.md`](firmware/LICENSE.md).

The web UI bundles the Geo, Roboto and Share Tech Mono fonts under the SIL
Open Font License 1.1, see [`internal/web/static/fonts/`](internal/web/static/fonts/).
