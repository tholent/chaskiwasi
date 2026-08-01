package chaskibridge

import (
	"fmt"
	"io"

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

// ResetDevice pulses the USB-CDC control lines the way esptool does, which is
// how the bench cuts power mid-exchange for C-4 without a hand on the cable.
//
// It is not a true power cut — RTC memory survives an esp_restart-class reset
// and does not survive a flat battery — so the bench also carries a documented
// manual power-cycle variant. What it does prove is the case C-4 names: the
// device stops between sending a request and applying the response, and the
// server's ack ring makes the resend produce exactly one email.
func ResetDevice(port io.ReadWriteCloser) error {
	p, ok := port.(serial.Port)
	if !ok {
		return fmt.Errorf("chaskibridge: %T is not a serial port", port)
	}
	// DTR low + RTS high asserts EN; releasing both lets the chip boot from
	// flash. The order matters: EN must be asserted while IO0 is left high, or
	// the chip comes up in the ROM bootloader instead of the application.
	if err := p.SetDTR(false); err != nil {
		return fmt.Errorf("chaskibridge: DTR: %w", err)
	}
	if err := p.SetRTS(true); err != nil {
		return fmt.Errorf("chaskibridge: RTS: %w", err)
	}
	if err := p.SetRTS(false); err != nil {
		return fmt.Errorf("chaskibridge: RTS release: %w", err)
	}
	return nil
}
