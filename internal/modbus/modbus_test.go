package modbus

import (
	"bytes"
	"errors"
	"math/rand"
	"net"
	"testing"
	"time"
)

func TestFrameRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		pdu := make([]byte, 1+r.Intn(maxPDU))
		r.Read(pdu)
		f := Frame{TxID: uint16(r.Intn(65536)), UnitID: byte(r.Intn(256)), PDU: pdu}
		g, err := ReadFrame(bytes.NewReader(f.Encode()))
		if err != nil || g.TxID != f.TxID || g.UnitID != f.UnitID || !bytes.Equal(g.PDU, f.PDU) {
			t.Fatalf("round trip %d: %v", i, err)
		}
	}
}

func TestMalformedFramesRejectedBeforeAllocation(t *testing.T) {
	for _, b := range [][]byte{
		{0, 1, 0, 1, 0, 6, 1, 3},       // protocol id 1
		{0, 1, 0, 0, 0xFF, 0xFF, 1, 3}, // length 65535
		{0, 1, 0, 0, 0, 1, 1},          // length 1: no function code
	} {
		if _, err := ReadFrame(bytes.NewReader(b)); !errors.Is(err, ErrFrame) {
			t.Fatalf("%x: expected ErrFrame, got %v", b, err)
		}
	}
}

func startServer(t *testing.T) (*Server, *Client) {
	m := NewDataModel(200)
	for i := range m.Holding {
		m.Holding[i] = uint16(1000 + i)
		m.Input[i] = uint16(2000 + i)
		m.Coils[i] = i%3 == 0
	}
	s := &Server{Model: m}
	addr, err := s.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, NewClient(addr, 1, time.Second)
}

func TestClientServerAllFunctions(t *testing.T) {
	s, c := startServer(t)
	v, err := c.ReadHoldingRegisters(10, 5)
	if err != nil || v[0] != 1010 || v[4] != 1014 {
		t.Fatalf("holding %v %v", v, err)
	}
	v, err = c.ReadInputRegisters(199, 1)
	if err != nil || v[0] != 2199 {
		t.Fatalf("input %v %v", v, err)
	}
	v, err = c.ReadCoils(0, 10)
	if err != nil || v[0] != 1 || v[1] != 0 || v[9] != 1 {
		t.Fatalf("coils %v %v", v, err)
	}
	if err := c.WriteSingleRegister(5, 42); err != nil || s.Model.Holding[5] != 42 {
		t.Fatal("write single", err)
	}
	if err := c.WriteMultipleRegisters(20, []uint16{7, 8, 9}); err != nil || s.Model.Holding[22] != 9 {
		t.Fatal("write multiple", err)
	}
	if err := c.WriteSingleCoil(1, true); err != nil || !s.Model.Coils[1] {
		t.Fatal("write coil", err)
	}
}

func TestExceptionsAndRecovery(t *testing.T) {
	s, c := startServer(t)
	_, err := c.ReadHoldingRegisters(190, 20)
	var ex *ExceptionError
	if !errors.As(err, &ex) || ex.Code != ExIllegalDataAddress {
		t.Fatalf("expected illegal address, got %v", err)
	}
	s.FailNext(1)
	if _, err := c.ReadHoldingRegisters(0, 1); !errors.As(err, &ex) || ex.Code != ExServerDeviceFail {
		t.Fatalf("expected device failure, got %v", err)
	}
	// exceptions keep the connection; a dropped connection is re-dialled
	if _, err := c.ReadHoldingRegisters(0, 1); err != nil {
		t.Fatal(err)
	}
	s.DropConnections()
	time.Sleep(20 * time.Millisecond)
	if _, err := c.ReadHoldingRegisters(0, 1); err == nil {
		t.Log("request on a dropped connection succeeded (kernel kept it)")
	}
	if _, err := c.ReadHoldingRegisters(0, 1); err != nil {
		t.Fatalf("no recovery after reconnect: %v", err)
	}
	if c.Reconnects < 2 {
		t.Fatalf("reconnects %d", c.Reconnects)
	}
}

func TestTimeoutOnSlowServer(t *testing.T) {
	s, c := startServer(t)
	s.Delay = 300 * time.Millisecond
	c.Timeout = 100 * time.Millisecond
	_, err := c.ReadHoldingRegisters(0, 1)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestServerDropsGarbage(t *testing.T) {
	s, _ := startServer(t)
	conn, err := net.Dial("tcp", s.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte{0, 1, 0, 9, 0, 6, 1, 3, 0, 0, 0, 1}) // protocol id 9
	conn.SetReadDeadline(time.Now().Add(time.Second))
	n, _ := conn.Read(make([]byte, 16))
	if n != 0 {
		t.Fatal("server answered a malformed frame")
	}
}
