// Command forge is a lean print server bridging a USB-attached Marlin 2.1
// printer to the network: an OctoPrint-compatible subset for slicers plus a
// small embedded web UI, sized to run comfortably on a Creality Wi-Fi Box
// (OpenWrt, MT7628, 128MB RAM, no FPU).
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"forge/internal/api"
	"forge/internal/printer"
	"forge/internal/serial"
	"forge/internal/web"
)

func main() {
	listen := flag.String("listen", ":80", "HTTP listen address")
	port := flag.String("port", "/dev/ttyUSB0", "serial port to the printer")
	baud := flag.Int("baud", 250000, "serial baud rate")
	dataDir := flag.String("data-dir", "/tmp/forge", "scratch dir for staged uploads")
	namesFile := flag.String("names-file", "/etc/forge/names.json", "long/short filename map")
	maxUploadMB := flag.Int("max-upload-mb", 40, "reject uploads larger than this many MiB")
	defaultMode := flag.String("default-mode", "sd", "default upload mode: sd or stream")
	bufsize := flag.Int("bufsize", 16, "outstanding-line budget; match Marlin's BUFSIZE")
	flag.Parse()

	// This box has ~128MB RAM and no swap worth mentioning; a lower GC
	// target trades some CPU for a much smaller resident heap, which
	// matters far more here than throughput.
	debug.SetGCPercent(50)

	log.SetFlags(0)
	log.SetOutput(os.Stdout)
	log.Printf("forge starting: port=%s baud=%d listen=%s mode=%s", *port, *baud, *listen, *defaultMode)

	sp, err := serial.Open(*port, *baud)
	if err != nil {
		log.Fatalf("forge: opening %s: %v", *port, err)
	}

	drv := printer.New(sp, *bufsize)
	names := printer.NewNameMap(*namesFile)
	drv.SetNames(names)
	drv.OnLine(func(string) {}) // console ring buffer is already recorded internally

	if err := drv.Handshake(5 * time.Second); err != nil {
		log.Printf("forge: handshake warning: %v", err)
	}
	drv.Start()

	// Ask Marlin to keep us updated without polling: M155 for temps, M27 S
	// for SD status, plus an M115 to learn capabilities and an initial
	// M20 L to seed the file list.
	drv.Send("M115")
	drv.Send("M155 S2")
	drv.Send("M27 S2")
	drv.Send("M20 L")

	mgr := printer.NewManager(drv, names)
	mode := printer.UploadMode(*defaultMode)
	if mode != printer.ModeSDUpload && mode != printer.ModeStream {
		log.Printf("forge: unknown -default-mode %q, using sd", *defaultMode)
		mode = printer.ModeSDUpload
	}

	srv := &api.Server{
		Mgr:     mgr,
		DataDir: *dataDir,
		MaxBody: int64(*maxUploadMB) * 1 << 20,
		Mode:    mode,
	}

	mux := http.NewServeMux()
	srv.Routes(mux)
	mux.Handle("/", web.Handler())

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadTimeout:       0, // uploads can be large/slow over USB-bridged serial boxes
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("forge listening on %s", *listen)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("forge: http server: %v", err)
	}
}
