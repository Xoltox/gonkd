// Command gonkd is a lean print server bridging a USB-attached Marlin 2.1
// printer to the network: an OctoPrint-compatible subset for slicers plus a
// small embedded web UI, sized to run comfortably on a Creality Wi-Fi Box
// (OpenWrt, MT7628, 128MB RAM, no FPU).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/Xoltox/gonkd/internal/api"
	"github.com/Xoltox/gonkd/internal/printer"
	"github.com/Xoltox/gonkd/internal/web"
)

func main() {
	listen := flag.String("listen", ":80", "HTTP listen address")
	port := flag.String("port", "/dev/ttyUSB0", "serial port to the printer")
	baud := flag.Int("baud", 250000, "serial baud rate")
	dataDir := flag.String("data-dir", "/tmp/gonkd", "scratch dir for staged uploads")
	namesFile := flag.String("names-file", "/etc/gonkd/names.json", "long/short filename map")
	maxUploadMB := flag.Int("max-upload-mb", 40, "reject uploads larger than this many MiB")
	defaultMode := flag.String("default-mode", "sd", "default upload mode: sd or stream")
	bufsize := flag.Int("bufsize", 16, "outstanding-line budget; match Marlin's BUFSIZE")
	allowHost := flag.String("allow-host", "", "extra Host header values to accept, comma-separated (IP literals and localhost are always accepted)")
	flag.Parse()

	// This box has ~128MB RAM and no swap worth mentioning; a lower GC
	// target trades some CPU for a much smaller resident heap, which
	// matters far more here than throughput.
	debug.SetGCPercent(50)

	log.SetFlags(0)
	log.SetOutput(os.Stdout)
	log.Printf("gonkd starting: port=%s baud=%d listen=%s mode=%s", *port, *baud, *listen, *defaultMode)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	names := printer.NewNameMap(*namesFile)
	mgr := printer.NewManager(names)

	// The printer may be off or unplugged; the link retries in the
	// background and the HTTP server comes up regardless.
	link := printer.NewLink(*port, *baud, *bufsize, names, mgr)
	linkDone := make(chan struct{})
	go func() {
		link.Run(ctx)
		close(linkDone)
	}()

	mode := printer.UploadMode(*defaultMode)
	if mode != printer.ModeSDUpload && mode != printer.ModeStream {
		log.Printf("gonkd: unknown -default-mode %q, using sd", *defaultMode)
		mode = printer.ModeSDUpload
	}

	var hosts []string
	for _, h := range strings.Split(*allowHost, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}

	srv := &api.Server{
		Mgr:        mgr,
		DataDir:    *dataDir,
		MaxBody:    int64(*maxUploadMB) << 20,
		Mode:       mode,
		AllowHosts: hosts,
	}

	mux := http.NewServeMux()
	srv.Routes(mux)
	mux.Handle("/", web.Handler())

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadTimeout:       0, // uploads can be large and slow on this box's Wi-Fi
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()

	log.Printf("gonkd listening on %s", *listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("gonkd: http server: %v", err)
	}
	// Let the link close the port cleanly (procd's term_timeout is 5s).
	select {
	case <-linkDone:
	case <-time.After(2 * time.Second):
	}
	log.Printf("gonkd stopped")
}
