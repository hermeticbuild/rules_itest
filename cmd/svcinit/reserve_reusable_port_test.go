package main

import (
	"context"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"rules_itest/svclib"
)

func TestReusablePortReservation(t *testing.T) {
	reservation, port, err := reserveReusablePort(0)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Close()

	unawareListener, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err == nil {
		unawareListener.Close()
		t.Fatal("listener without a reusable-port option unexpectedly claimed the reserved port")
	}

	lc := net.ListenConfig{
		Control: func(network, address string, conn syscall.RawConn) error {
			var setSockoptErr error
			err := conn.Control(func(fd uintptr) {
				setSockoptErr = setSockoptsForPortAssignment(fd, true)
			})
			if err != nil {
				return err
			}
			return setSockoptErr
		},
	}
	listener, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("listen on reserved port: %v", err)
	}
	defer listener.Close()

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := listener.Accept()
		// Keep the accepted socket open until Dial returns. On Linux it inherits
		// zero-linger, so closing it here can reset the client during Dial.
		accepted <- acceptResult{conn: conn, err: err}
	}()

	conn, err := net.DialTimeout("tcp4", "127.0.0.1:"+port, time.Second)
	if err != nil {
		t.Fatalf("dial service listener: %v", err)
	}
	conn.Close()

	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatalf("accept from service listener: %v", result.err)
		}
		result.conn.Close()
	case <-time.After(time.Second):
		t.Fatal("connection was not accepted by the service listener")
	}

	t.Setenv("ASSIGNED_PORTS", "")
	for _, reusable := range []bool{false, true} {
		ports, reserved, err := assignPorts(map[string]svclib.ServiceSpec{
			"service": {NamedPorts: map[string]string{"occupied": port}, SoReuseportAware: reusable},
		})
		if err := closePorts(reserved); err != nil {
			t.Error(err)
		}
		if err == nil {
			t.Errorf("reuse-aware=%t: assigned occupied %s: %v", reusable, listener.Addr(), ports)
		}
	}
}

func TestFailedAssignmentReleasesPorts(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().(*net.TCPAddr)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(address.Port)
	t.Setenv("ASSIGNED_PORTS", "")
	for _, reusable := range []bool{false, true} {
		_, reserved, err := assignPorts(map[string]svclib.ServiceSpec{
			"service": {NamedPorts: map[string]string{"first": port, "second": port}, SoReuseportAware: reusable},
		})
		t.Cleanup(func() {
			if err := closePorts(reserved); err != nil {
				t.Error(err)
			}
		})
		if err == nil {
			t.Fatalf("reuse-aware=%t: assigned the same port twice", reusable)
		}
		listener, err := net.Listen("tcp4", address.String())
		if err != nil {
			t.Fatalf("reuse-aware=%t: failed assignment did not release %s: %v", reusable, address, err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
