# Building the matching Marlin firmware

Gonk'd expects a Marlin build with a few host-friendly features switched on.
This folder holds the exact configuration used on the reference machine:

- Ender 3 V1, Creality 4.2.2 board (STM32F103RET6, stock Creality bootloader,
  A4988-class drivers, CH340 USB)
- Marlin 2.1.2.7

The config files here are licensed GPL-3.0 (see [LICENSE.md](LICENSE.md)).
They are NOT covered by the MIT license of the rest of the repo.

> These values (PID, steps, directions, bed size) are tuned for one specific
> Ender 3 V1 on a 4.2.2 board. If your printer differs, treat this as a
> reference and port the host-related options (listed below) into your own
> config instead of flashing it blindly.

## Versions

- Marlin firmware: tag `2.1.2.7` (https://github.com/MarlinFirmware/Marlin)
- MarlinFirmware/Configurations: tag `2.1.2.7`
- Starting point: `config/examples/Creality/Ender-3/CrealityV422`
  (`_Statusscreen.h` is left stock and is not included here)

## Build

PlatformIO environment: `STM32F103RE_creality` (28KiB bootloader offset,
matches `MOTHERBOARD BOARD_CREALITY_V4`).

```bash
git clone --branch 2.1.2.7 --depth 1 https://github.com/MarlinFirmware/Marlin.git
cp Configuration.h Configuration_adv.h _Bootscreen.h Marlin/Marlin/
cd Marlin
pio run -e STM32F103RE_creality
# output: .pio/build/STM32F103RE_creality/firmware-<timestamp>.bin
```

Tip: build inside a Linux filesystem (not a Windows drive mounted into WSL),
it is much faster. PlatformIO in a Python venv works fine.

Result on the reference build: flash 166920 / 524288 bytes (31.8%),
RAM 20244 / 65536 bytes (30.9%). Plenty of headroom.

## Flashing

Copy the newest `firmware-<timestamp>.bin` to the root of the printer's
microSD card and power-cycle. The stock Creality bootloader refuses a file
whose name matches the last one it flashed; the build script appends a
timestamp, so every build is accepted without renaming. After flashing:
`M502` then `M500` to reset EEPROM to the new defaults.

## Changes vs the CrealityV422 example

### Configuration.h

- `BAUDRATE` 115200 -> 250000
- Hotend PID (`DEFAULT_KP/KI/KD`) 21.73/1.54/76.55 -> 24.087/1.245/116.522
  (tuned for this machine)
- `PIDTEMPBED` enabled, bed PID 66.745/1.606/693.318 (tuned for this machine)
- Thermal protection: left enabled (stock)
- `MESH_BED_LEVELING` enabled (no probe on this machine), 5x5 grid,
  `MESH_INSET` 10mm (stock), `LCD_BED_LEVELING`, `MESH_EDIT_MENU`,
  `RESTORE_LEVELING_AFTER_G28`
- `S_CURVE_ACCELERATION` disabled (traded for `LIN_ADVANCE`, less ISR work)
- Custom boot screen bitmap in `_Bootscreen.h`
- Stepper directions and steps/mm: stock values, verified against a
  known-good previous config for the same machine; unchanged

### Configuration_adv.h

Host-facing options (the ones Gonk'd relies on):

| Option | Value | Why |
|---|---|---|
| `SDCARD_READONLY` | off | needed to write uploads to SD |
| `BINARY_FILE_TRANSFER` | on | faster uploads (Gonk'd support is WIP) |
| `BUFSIZE` | 16 | more queued commands (match `-bufsize`) |
| `BLOCK_BUFFER_SIZE` | 64 | more planner moves to ride out host stalls |
| `RX_BUFFER_SIZE` | 2048 | same, on the serial side |
| `TX_BUFFER_SIZE` | 64 | |
| `ADVANCED_OK` | on | `ok N P B` for flow control |
| `SERIAL_OVERRUN_PROTECTION` | on (stock) | |
| `EMERGENCY_PARSER` | on | `M112`/`M108`/`M410` skip the queue |
| `HOST_ACTION_COMMANDS` | on | |
| `SET_PROGRESS_MANUALLY` | on | `M73` progress |
| `AUTO_REPORT_TEMPERATURES` | on (stock) | `M155`, no polling |
| `AUTO_REPORT_SD_STATUS` | on | `M27 S`, no polling |
| `EXTENDED_CAPABILITIES_REPORT` | on (stock) | |
| `LONG_FILENAME_HOST_SUPPORT` | on | `M20 L` long names |
| `SDCARD_SORT_ALPHA` | on | |

Printer-side niceties:

- `BABYSTEPPING` (stock) + `BABYSTEP_ALWAYS_AVAILABLE` + `BABYSTEP_DISPLAY_TOTAL`,
  `DOUBLECLICK_FOR_Z_BABYSTEPPING` (stock)
- `LIN_ADVANCE` on with `ADVANCE_K 0` (calibrate your own K)
- `ADAPTIVE_STEP_SMOOTHING` on
- `ARC_SUPPORT` on (stock); turn on arc fitting in your slicer

Note: with mesh bed leveling and no probe, `M500` does not persist `M290`
babysteps. The persistent equivalent is `G29 S4 Z<offset>` followed by `M500`.
