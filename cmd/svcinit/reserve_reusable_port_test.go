package main

import (
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"rules_itest/svclib"
)

func TestReusablePortReservation(t *testing.T) {
	t.Setenv("ASSIGNED_PORTS", "")
	for _, tc := range []struct{ family, network, host string }{
		{"ipv4", "tcp4", "127.0.0.1"},
		{"ipv6", "tcp6", "::1"},
	} {
		t.Run(tc.family, func(t *testing.T) {
			if tc.family == "ipv6" {
				requireIPv6Loopback(t)
			}
			reservation, port, err := reserveServicePort(tc.family, "0", true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := closePorts(reservation); err != nil {
					t.Error(err)
				}
			})

			address := net.JoinHostPort(tc.host, port)
			unaware, err := net.Listen(tc.network, address)
			if err == nil {
				unaware.Close()
				t.Fatal("listener without a reusable-port option unexpectedly claimed the reserved port")
			}

			lc := net.ListenConfig{Control: func(_, _ string, conn syscall.RawConn) error {
				var optionErr error
				if err := conn.Control(func(fd uintptr) {
					optionErr = syscall.SetsockoptInt(socketHandle(fd), syscall.SOL_SOCKET, reusablePortOption, 1)
				}); err != nil {
					return err
				}
				return optionErr
			}}
			listener, err := lc.Listen(t.Context(), tc.network, address)
			if err != nil {
				t.Fatalf("listen on reserved port: %v", err)
			}
			defer listener.Close()

			conn, err := net.DialTimeout(tc.network, address, time.Second)
			if err != nil {
				t.Fatalf("dial service listener: %v", err)
			}
			defer conn.Close()
			if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			accepted, err := listener.Accept()
			if err != nil {
				t.Fatalf("accept from service listener: %v", err)
			}
			accepted.Close()

			for _, reusable := range []bool{false, true} {
				ports, reserved, err := assignPorts(map[string]svclib.ServiceSpec{
					"service": {NamedPorts: map[string]string{"occupied": port}, PortFamily: tc.family, SoReuseportAware: reusable},
				})
				if closeErr := closePorts(reserved); closeErr != nil {
					t.Error(closeErr)
				}
				if err == nil {
					t.Errorf("reuse-aware=%t: assigned occupied %s: %v", reusable, address, ports)
				}
			}
		})
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
