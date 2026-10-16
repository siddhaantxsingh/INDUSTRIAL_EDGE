// Package modbus implements Modbus TCP (MBAP framing) from the Modbus
// Application Protocol Specification V1.1b3 and Messaging on TCP/IP
// Implementation Guide V1.0b: a client with transaction-ID matching,
// timeouts and reconnect, and a server used by the PLC simulator.
//
// Supported function codes: 0x01 read coils, 0x03 read holding registers,
// 0x04 read input registers, 0x05 write single coil, 0x06 write single
// register, 0x10 write multiple registers. Exception responses are decoded
// into *ExceptionError. Frames with an inconsistent length, wrong protocol
// id or oversized payload are rejected before allocation.
package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Function codes.
const (
	FuncReadCoils         = 0x01
	FuncReadHolding       = 0x03
	FuncReadInput         = 0x04
	FuncWriteSingleCoil   = 0x05
	FuncWriteSingleReg    = 0x06
	FuncWriteMultipleRegs = 0x10
	MaxReadRegisters      = 125
	MaxWriteRegisters     = 123
	MaxReadCoils          = 2000
	maxPDU                = 253
	mbapLen               = 7
)

// Exception codes.
const (
	ExIllegalFunction    = 0x01
	ExIllegalDataAddress = 0x02
	ExIllegalDataValue   = 0x03
	ExServerDeviceFail   = 0x04
)

// ExceptionError is a Modbus exception response.
type ExceptionError struct {
	Function byte
	Code     byte
}

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("modbus exception 0x%02x for function 0x%02x", e.Code, e.Function)
}

// ErrFrame reports a malformed frame.
var ErrFrame = errors.New("modbus: malformed frame")

// Frame is one MBAP-framed ADU.
type Frame struct {
	TxID   uint16
	UnitID byte
	PDU    []byte // function code + data
}

// Encode serialises the frame.
func (f *Frame) Encode() []byte {
	b := make([]byte, mbapLen+len(f.PDU))
	binary.BigEndian.PutUint16(b[0:], f.TxID)
	binary.BigEndian.PutUint16(b[2:], 0) // protocol id
	binary.BigEndian.PutUint16(b[4:], uint16(len(f.PDU)+1))
	b[6] = f.UnitID
	copy(b[7:], f.PDU)
	return b
}

// ReadFrame reads exactly one frame, validating the MBAP header first.
func ReadFrame(r io.Reader) (*Frame, error) {
	var h [mbapLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(h[2:]) != 0 {
		return nil, fmt.Errorf("%w: protocol id %d", ErrFrame, binary.BigEndian.Uint16(h[2:]))
	}
	n := int(binary.BigEndian.Uint16(h[4:]))
	if n < 2 || n-1 > maxPDU {
		return nil, fmt.Errorf("%w: length %d", ErrFrame, n)
	}
	pdu := make([]byte, n-1)
	if _, err := io.ReadFull(r, pdu); err != nil {
		return nil, err
	}
	return &Frame{TxID: binary.BigEndian.Uint16(h[0:]), UnitID: h[6], PDU: pdu}, nil
}

// ---------------------------------------------------------------- requests

// ReadRequest builds the PDU for function 0x01/0x03/0x04.
func ReadRequest(fn byte, addr, qty uint16) []byte {
	p := []byte{fn, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(p[1:], addr)
	binary.BigEndian.PutUint16(p[3:], qty)
	return p
}

// WriteSingleRequest builds 0x05 / 0x06.
func WriteSingleRequest(fn byte, addr, val uint16) []byte {
	return ReadRequest(fn, addr, val)
}

// WriteMultipleRequest builds 0x10.
func WriteMultipleRequest(addr uint16, vals []uint16) []byte {
	p := make([]byte, 6+2*len(vals))
	p[0] = FuncWriteMultipleRegs
	binary.BigEndian.PutUint16(p[1:], addr)
	binary.BigEndian.PutUint16(p[3:], uint16(len(vals)))
	p[5] = byte(2 * len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint16(p[6+2*i:], v)
	}
	return p
}

// ParseResponse validates a response PDU against the request it answers and
// returns register values (0x03/0x04), coil states packed as 0/1 values
// (0x01), or nil for writes.
func ParseResponse(req, resp []byte) ([]uint16, error) {
	if len(resp) < 2 {
		return nil, fmt.Errorf("%w: short response", ErrFrame)
	}
	fn := req[0]
	if resp[0] == fn|0x80 {
		return nil, &ExceptionError{Function: fn, Code: resp[1]}
	}
	if resp[0] != fn {
		return nil, fmt.Errorf("%w: function 0x%02x in response to 0x%02x", ErrFrame, resp[0], fn)
	}
	switch fn {
	case FuncReadHolding, FuncReadInput:
		qty := int(binary.BigEndian.Uint16(req[3:]))
		if int(resp[1]) != 2*qty || len(resp) != 2+2*qty {
			return nil, fmt.Errorf("%w: byte count %d for %d registers", ErrFrame, resp[1], qty)
		}
		out := make([]uint16, qty)
		for i := range out {
			out[i] = binary.BigEndian.Uint16(resp[2+2*i:])
		}
		return out, nil
	case FuncReadCoils:
		qty := int(binary.BigEndian.Uint16(req[3:]))
		nb := (qty + 7) / 8
		if int(resp[1]) != nb || len(resp) != 2+nb {
			return nil, fmt.Errorf("%w: coil byte count %d for %d coils", ErrFrame, resp[1], qty)
		}
		out := make([]uint16, qty)
		for i := range out {
			out[i] = uint16(resp[2+i/8]>>(i%8)) & 1
		}
		return out, nil
	case FuncWriteSingleCoil, FuncWriteSingleReg:
		if len(resp) != 5 || string(resp) != string(req[:5]) {
			return nil, fmt.Errorf("%w: write echo mismatch", ErrFrame)
		}
		return nil, nil
	case FuncWriteMultipleRegs:
		if len(resp) != 5 || string(resp[1:5]) != string(req[1:5]) {
			return nil, fmt.Errorf("%w: write-multiple echo mismatch", ErrFrame)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("%w: unsupported function 0x%02x", ErrFrame, fn)
}
