// Command chaskibridge forwards a dev-build device's framed USB-CDC sync
// traffic to a Wasi instance. See package chaskibridge for what it is and why
// it deliberately does nothing clever.
//
// Wave 0 scaffold: flag surface only; Wave 2A implements Run.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tholent/chaskiwasi/tools/chaskibridge"
)

func main() {
	var cfg chaskibridge.Config
	flag.StringVar(&cfg.SerialPort, "port", "/dev/ttyACM0", "device USB-CDC port")
	flag.StringVar(&cfg.WasiURL, "wasi", "https://localhost:8443/sync", "Wasi sync endpoint")
	flag.BoolVar(&cfg.InsecureSkipVerify, "insecure", false,
		"accept the bench stack's self-signed certificate (developer machine only; "+
			"the device's own TLS is pinned in the modem and never relaxed)")
	flag.DurationVar(&cfg.Timeout, "timeout", chaskibridge.DefaultTimeout, "per-request timeout")
	flag.Parse()

	fmt.Fprintln(os.Stderr, "chaskibridge: not implemented yet (Wave 2A owns the serial loop)")
	os.Exit(1)
}
