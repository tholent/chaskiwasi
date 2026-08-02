// Command chaskibridge forwards a dev-build device's framed USB-CDC sync
// traffic to a Wasi instance. See package chaskibridge for what it is and why
// it deliberately does nothing clever.
//
// Typical bench session against the compose stack:
//
//	make up
//	docker compose -f deploy/compose.dev.yml cp wasi:/config/tls/ca.crt /tmp/wasi-ca.crt
//	go run ./tools/chaskibridge/cmd/chaskibridge \
//	    -port /dev/ttyACM0 -wasi https://127.0.0.1:18443/sync -ca /tmp/wasi-ca.crt \
//	    -console device.log
//
// device.log collects everything on the link that is not a frame — the dev
// build's serial output, since the console shares the USB-CDC peripheral. That
// capture is what C-19 greps (D-7).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tholent/chaskiwasi/tools/chaskibridge"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chaskibridge:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg chaskibridge.Config
	flag.StringVar(&cfg.SerialPort, "port", "/dev/ttyACM0", "device USB-CDC port")
	flag.StringVar(&cfg.WasiURL, "wasi", "https://localhost:8443/sync", "Wasi sync endpoint")
	flag.StringVar(&cfg.CAFile, "ca", "",
		"PEM bundle to verify Wasi with — the compose stack's ca.crt. Without it "+
			"(and without -insecure) a run exercises C-7's trust-failure case")
	flag.BoolVar(&cfg.InsecureSkipVerify, "insecure", false,
		"accept the bench stack's self-signed certificate (developer machine only; "+
			"the device's own TLS is pinned in the modem and never relaxed)")
	flag.DurationVar(&cfg.Timeout, "timeout", chaskibridge.DefaultTimeout, "per-request timeout")
	consolePath := flag.String("console", "",
		"write the device's non-frame serial output here ('-' for stdout)")
	verbose := flag.Bool("v", false, "debug logging (still content-free — D-7)")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	console, closeConsole, err := openConsole(*consolePath)
	if err != nil {
		return err
	}
	defer closeConsole()
	cfg.Console = console

	bridge, err := chaskibridge.New(cfg)
	if err != nil {
		return err
	}
	link, err := chaskibridge.OpenSerial(cfg)
	if err != nil {
		return err
	}
	defer link.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg.Logger.Info("bridge up", "port", cfg.SerialPort, "wasi", cfg.WasiURL,
		"verify_tls", !cfg.InsecureSkipVerify)
	started := time.Now()
	serveErr := bridge.Serve(ctx, link)

	s := bridge.Stats()
	cfg.Logger.Info("bridge down", "uptime_s", int(time.Since(started).Seconds()),
		"frames", s.Frames, "requests", s.Requests, "responses", s.Responses,
		"resyncs", s.Resyncs, "torn", s.Torn, "transport_fails", s.TransportFails,
		"tls_trust_fails", s.TLSTrustFails, "bytes_in", s.BytesIn, "bytes_out", s.BytesOut)
	return serveErr
}

func openConsole(path string) (io.Writer, func(), error) {
	switch path {
	case "":
		return nil, func() {}, nil
	case "-":
		return os.Stdout, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the console capture: %w", err)
	}
	return f, func() { f.Close() }, nil
}
