// Command wasi is the per-device server. One container per Chaski device: the
// container *is* the device's identity, so there is no device_id anywhere in the
// code, config, or state, and every file is singular (wasi-server-plan §2).
//
// Subcommands:
//
//	serve      run both TLS listeners (§12.1)
//	backup     copy /data excluding kipu/ to the backup volume (§3)
//	useradd    create a guardian account (§9.2)
//	contacts   list maintenance with the server stopped (§14)
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve", "backup", "useradd", "contacts":
		fmt.Fprintf(os.Stderr, "wasi: %s is not implemented yet\n", os.Args[1])
		os.Exit(1)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: wasi <command>

  serve      run the device and guardian listeners
  backup     copy /data (excluding kipu/) to the backup volume
  useradd    create a guardian account
  contacts   contact list maintenance (server must be stopped)
`)
}
