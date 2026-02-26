package main

import (
	"os"

	"go.bug.st/serial"
)

// SerialPort wraps a go.bug.st/serial port with a frame-send helper.
type SerialPort struct {
	port serial.Port
}

// OpenSerial opens the named serial device at the given baud rate.
// Calls log.Fatal on error.
func OpenSerial(name string, baud int) *SerialPort {
	mode := &serial.Mode{BaudRate: baud}
	p, err := serial.Open(name, mode)
	if err != nil {
		logger.Error("serial: failed to open port", "device", name, "baud", baud, "err", err)
		os.Exit(1)
	}
	logger.Info("serial: port opened", "device", name, "baud", baud)
	return &SerialPort{port: p}
}

// SendFrame encodes and writes a Frame to the serial port.
func (s *SerialPort) SendFrame(f Frame) {
	data := f.Encode()
	n, err := s.port.Write(data)
	if err != nil {
		logger.Error("serial: write error", "err", err)
		return
	}
	logger.Info("serial: frame sent", "bytes", n, "seq", f.Seq, "strum_mask", f.StrumMask)
}

// Close closes the underlying serial port.
func (s *SerialPort) Close() {
	logger.Info("serial: closing port")
	_ = s.port.Close()
}
