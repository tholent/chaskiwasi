package main

import "errors"

// dataLockName is the sentinel file every `wasi` process that touches
// /data's server-owned files locks for as long as it holds them open. It
// carries no data of its own — it is never read, and its presence or
// absence in a restored backup means nothing either way (backup.go excludes
// it explicitly, same as atomicfile's own temp files, for exactly that
// reason). See lock_unix.go's acquireDataLock for what it protects and why.
const dataLockName = ".wasi.lock"

// errDataDirBusy is what acquireDataLock returns when another wasi process
// already holds the lock on this data directory. Callers turn it into an
// operator-facing message naming the command that refused and why.
var errDataDirBusy = errors.New("another wasi process already holds this data directory")
