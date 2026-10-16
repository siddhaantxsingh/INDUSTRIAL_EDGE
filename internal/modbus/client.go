package modbus

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client is a Modbus TCP client. It is safe for concurrent use; requests
// are serialised on one connection (most PLCs accept one outstanding
// transaction). A broken connection is re-dialled on the next request.
type Client struct {
	Addr    string
	UnitID  byte
	Timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
	txID uint16

	Requests, Errors, Reconnects uint64
}

// NewClient returns a client for addr ("host:port").
func NewClient(addr string, unit byte, timeout time.Duration) *Client {
	return &Client{Addr: addr, UnitID: unit, Timeout: timeout}
}

func (c *Client) dial() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("tcp", c.Addr, c.Timeout)
	if err != nil {
		return err
	}
	c.conn = conn
	c.Reconnects++
	return nil
}

// Close drops the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

func (c *Client) do(req []byte) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Requests++
	if err := c.dial(); err != nil {
		c.Errors++
		return nil, err
	}
	c.txID++
	f := Frame{TxID: c.txID, UnitID: c.UnitID, PDU: req}
	_ = c.conn.SetDeadline(time.Now().Add(c.Timeout))
	if _, err := c.conn.Write(f.Encode()); err != nil {
		c.drop()
		return nil, err
	}
	for {
		resp, err := ReadFrame(c.conn)
		if err != nil {
			c.drop()
			return nil, err
		}
		if resp.TxID != f.TxID {
			// a late answer to an earlier, timed-out request: skip it
			if int16(resp.TxID-f.TxID) < 0 {
				continue
			}
			c.drop()
			return nil, fmt.Errorf("%w: transaction id %d, expected %d", ErrFrame, resp.TxID, f.TxID)
		}
		vals, err := ParseResponse(req, resp.PDU)
		if err != nil {
			c.Errors++
			var ex *ExceptionError
			if !errors.As(err, &ex) {
				c.drop()
			}
		}
		return vals, err
	}
}

func (c *Client) drop() {
	c.Errors++
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// ReadHoldingRegisters reads qty registers starting at addr (function 0x03).
func (c *Client) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	if qty == 0 || qty > MaxReadRegisters {
		return nil, fmt.Errorf("modbus: quantity %d out of range", qty)
	}
	return c.do(ReadRequest(FuncReadHolding, addr, qty))
}

// ReadInputRegisters reads qty input registers (function 0x04).
func (c *Client) ReadInputRegisters(addr, qty uint16) ([]uint16, error) {
	if qty == 0 || qty > MaxReadRegisters {
		return nil, fmt.Errorf("modbus: quantity %d out of range", qty)
	}
	return c.do(ReadRequest(FuncReadInput, addr, qty))
}

// ReadCoils reads qty coils (function 0x01); each value is 0 or 1.
func (c *Client) ReadCoils(addr, qty uint16) ([]uint16, error) {
	if qty == 0 || qty > MaxReadCoils {
		return nil, fmt.Errorf("modbus: quantity %d out of range", qty)
	}
	return c.do(ReadRequest(FuncReadCoils, addr, qty))
}

// WriteSingleRegister writes one holding register (function 0x06).
func (c *Client) WriteSingleRegister(addr, val uint16) error {
	_, err := c.do(WriteSingleRequest(FuncWriteSingleReg, addr, val))
	return err
}

// WriteSingleCoil writes one coil (function 0x05).
func (c *Client) WriteSingleCoil(addr uint16, on bool) error {
	v := uint16(0)
	if on {
		v = 0xFF00
	}
	_, err := c.do(WriteSingleRequest(FuncWriteSingleCoil, addr, v))
	return err
}

// WriteMultipleRegisters writes consecutive holding registers (function 0x10).
func (c *Client) WriteMultipleRegisters(addr uint16, vals []uint16) error {
	if len(vals) == 0 || len(vals) > MaxWriteRegisters {
		return fmt.Errorf("modbus: quantity %d out of range", len(vals))
	}
	_, err := c.do(WriteMultipleRequest(addr, vals))
	return err
}
