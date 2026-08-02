package chaskibridge

import (
	"fmt"
	"io"
	"time"

	"go.bug.st/serial"
)

// OpenSerial opens the device's USB-CDC node.
//
// Serial I/O is isolated here, behind io.ReadWriteCloser, so everything the
// bridge actually decides — framing, forwarding, classification — is tested
// against an in-memory pipe with no hardware attached. That is the same
// argument the firmware's SerialLink seam makes on the other end of the cable.
//
// The read timeout is deliberately NoTimeout: a bridge session is a long
// silence punctuated by frames, and a timeout would turn each silence into a
// zero-byte read the frame reader has no way to distinguish from a broken
// stream. Serve unblocks by closing the port when its context is cancelled.
func OpenSerial(cfg Config) (io.ReadWriteCloser, error) {
	if cfg.SerialPort == "" {
		return nil, fmt.Errorf("chaskibridge: no serial port configured")
	}
	baud := cfg.BaudRate
	if baud == 0 {
		baud = DefaultBaudRate
	}
	port, err := serial.Open(cfg.SerialPort, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("chaskibridge: opening %s: %w", cfg.SerialPort, err)
	}
	if err := port.SetReadTimeout(serial.NoTimeout); err != nil {
		port.Close()
		return nil, fmt.Errorf("chaskibridge: configuring %s: %w", cfg.SerialPort, err)
	}
	return port, nil
}

// resetPulseMs holds EN asserted long enough for the chip to see it. esptool
// uses 100 ms on this path and the value is not worth being clever about: too
// short is a reset that silently does not happen.
const resetPulseMs = 100

// ResetDevice pulses the USB-CDC control lines the way esptool's hard reset
// does, restarting the application without entering the ROM bootloader.
//
// Two things it is not. It is not a true power cut: RTC memory survives an
// esp_restart-class reset and does not survive a flat battery, so the bench
// also carries a documented manual power-cycle variant. And it is not the
// bench's primary C-4 mechanism — the `sync` control command's cut_at field
// restarts the device at a named §5.2 step, which is deterministic where
// timing a line pulse against an in-flight HTTP round trip is not.
//
// On the ESP32-S3's built-in USB-Serial-JTAG the peripheral IS the USB device,
// so a reset drops the connection: the port node disappears and re-enumerates.
// The caller must close this handle and reopen with OpenSerialWait.
func ResetDevice(port io.ReadWriteCloser) error {
	p, ok := port.(serial.Port)
	if !ok {
		return fmt.Errorf("chaskibridge: %T is not a serial port", port)
	}
	// RTS drives EN, DTR drives IO0. Asserting RTS alone pulls EN low with IO0
	// left high, which is a plain restart; dropping DTR before releasing RTS is
	// what keeps the chip out of the download bootloader on release.
	if err := p.SetRTS(true); err != nil {
		return fmt.Errorf("chaskibridge: asserting EN: %w", err)
	}
	time.Sleep(resetPulseMs * time.Millisecond)
	if err := p.SetDTR(false); err != nil {
		return fmt.Errorf("chaskibridge: releasing IO0: %w", err)
	}
	if err := p.SetRTS(false); err != nil {
		return fmt.Errorf("chaskibridge: releasing EN: %w", err)
	}
	return nil
}

// OpenSerialWait opens the port, retrying until it appears or the deadline
// passes. A device that has just reset is re-enumerating on USB, so its node is
// legitimately absent for a moment; treating that as a failure would make every
// reset a flaky test rather than a step.
func OpenSerialWait(cfg Config, timeout time.Duration) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(timeout)
	for {
		link, err := OpenSerial(cfg)
		if err == nil {
			return link, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("chaskibridge: %s did not come back within %s: %w",
				cfg.SerialPort, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
