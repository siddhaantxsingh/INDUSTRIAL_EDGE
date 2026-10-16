package modbus

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// DataModel is the register space served by a Server.
type DataModel struct {
	mu      sync.RWMutex
	Holding []uint16
	Input   []uint16
	Coils   []bool
	// optional hook after a client write (address, values)
	OnWrite func(addr uint16, vals []uint16)
}

// NewDataModel allocates n of each register type.
func NewDataModel(n int) *DataModel {
	return &DataModel{Holding: make([]uint16, n), Input: make([]uint16, n), Coils: make([]bool, n)}
}

// Update runs fn with the model locked for writing (used by the simulator).
func (m *DataModel) Update(fn func(m *DataModel)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

// Server serves a DataModel over Modbus TCP. Delay adds artificial response
// latency; FailNext forces the next n requests to return exception 0x04.
type Server struct {
	Model *DataModel
	Delay time.Duration

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	failNext int
	wg       sync.WaitGroup
}

// Listen starts serving on addr ("127.0.0.1:0" for an ephemeral port).
func (s *Server) Listen(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.ln = ln
	s.conns = map[net.Conn]struct{}{}
	s.wg.Add(1)
	go s.accept()
	return ln.Addr().String(), nil
}

// FailNext makes the next n requests fail with a server-device-failure exception.
func (s *Server) FailNext(n int) {
	s.mu.Lock()
	s.failNext = n
	s.mu.Unlock()
}

// DropConnections closes all client connections (simulates a network blip).
func (s *Server) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.Close()
	}
}

// Close stops the server.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.DropConnections()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	for {
		f, err := ReadFrame(c)
		if err != nil {
			if !errors.Is(err, io.EOF) && errors.Is(err, ErrFrame) {
				return // malformed: drop the connection
			}
			return
		}
		if s.Delay > 0 {
			time.Sleep(s.Delay)
		}
		resp := &Frame{TxID: f.TxID, UnitID: f.UnitID, PDU: s.handle(f.PDU)}
		if _, err := c.Write(resp.Encode()); err != nil {
			return
		}
	}
}

func exception(fn, code byte) []byte { return []byte{fn | 0x80, code} }

func (s *Server) handle(p []byte) []byte {
	fn := p[0]
	s.mu.Lock()
	if s.failNext > 0 {
		s.failNext--
		s.mu.Unlock()
		return exception(fn, ExServerDeviceFail)
	}
	s.mu.Unlock()
	m := s.Model
	switch fn {
	case FuncReadHolding, FuncReadInput, FuncReadCoils:
		if len(p) != 5 {
			return exception(fn, ExIllegalDataValue)
		}
		addr, qty := int(binary.BigEndian.Uint16(p[1:])), int(binary.BigEndian.Uint16(p[3:]))
		m.mu.RLock()
		defer m.mu.RUnlock()
		if fn == FuncReadCoils {
			if qty < 1 || qty > MaxReadCoils {
				return exception(fn, ExIllegalDataValue)
			}
			if addr+qty > len(m.Coils) {
				return exception(fn, ExIllegalDataAddress)
			}
			nb := (qty + 7) / 8
			out := make([]byte, 2+nb)
			out[0], out[1] = fn, byte(nb)
			for i := 0; i < qty; i++ {
				if m.Coils[addr+i] {
					out[2+i/8] |= 1 << (i % 8)
				}
			}
			return out
		}
		if qty < 1 || qty > MaxReadRegisters {
			return exception(fn, ExIllegalDataValue)
		}
		regs := m.Holding
		if fn == FuncReadInput {
			regs = m.Input
		}
		if addr+qty > len(regs) {
			return exception(fn, ExIllegalDataAddress)
		}
		out := make([]byte, 2+2*qty)
		out[0], out[1] = fn, byte(2*qty)
		for i := 0; i < qty; i++ {
			binary.BigEndian.PutUint16(out[2+2*i:], regs[addr+i])
		}
		return out
	case FuncWriteSingleReg, FuncWriteSingleCoil:
		if len(p) != 5 {
			return exception(fn, ExIllegalDataValue)
		}
		addr, val := int(binary.BigEndian.Uint16(p[1:])), binary.BigEndian.Uint16(p[3:])
		m.mu.Lock()
		if fn == FuncWriteSingleCoil {
			if val != 0 && val != 0xFF00 {
				m.mu.Unlock()
				return exception(fn, ExIllegalDataValue)
			}
			if addr >= len(m.Coils) {
				m.mu.Unlock()
				return exception(fn, ExIllegalDataAddress)
			}
			m.Coils[addr] = val == 0xFF00
		} else {
			if addr >= len(m.Holding) {
				m.mu.Unlock()
				return exception(fn, ExIllegalDataAddress)
			}
			m.Holding[addr] = val
		}
		hook := m.OnWrite
		m.mu.Unlock()
		if hook != nil {
			hook(uint16(addr), []uint16{val})
		}
		return append([]byte(nil), p...)
	case FuncWriteMultipleRegs:
		if len(p) < 6 {
			return exception(fn, ExIllegalDataValue)
		}
		addr, qty, nb := int(binary.BigEndian.Uint16(p[1:])), int(binary.BigEndian.Uint16(p[3:])), int(p[5])
		if qty < 1 || qty > MaxWriteRegisters || nb != 2*qty || len(p) != 6+nb {
			return exception(fn, ExIllegalDataValue)
		}
		m.mu.Lock()
		if addr+qty > len(m.Holding) {
			m.mu.Unlock()
			return exception(fn, ExIllegalDataAddress)
		}
		vals := make([]uint16, qty)
		for i := 0; i < qty; i++ {
			vals[i] = binary.BigEndian.Uint16(p[6+2*i:])
			m.Holding[addr+i] = vals[i]
		}
		hook := m.OnWrite
		m.mu.Unlock()
		if hook != nil {
			hook(uint16(addr), vals)
		}
		return p[:5]
	}
	return exception(fn, ExIllegalFunction)
}
