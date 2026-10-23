package main

import (
	"net"
	"strings"
	"testing"

	"rules_itest/svclib"
)

func requireIPv6Loopback(t *testing.T) {
	t.Helper()
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoopbackPortAssignment(t *testing.T) {
	requireIPv6Loopback(t)
	// Start with a port available on both loopbacks, then leave IPv4 occupied.
	occupied, portString, err := reserveServicePort("dual-stack", "0", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closePorts(occupied); err != nil {
			t.Error(err)
		}
	}()
	if len(occupied) != 2 {
		t.Fatalf("expected two loopback reservations, got %d", len(occupied))
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		address := net.JoinHostPort(host, portString)
		listener, err := net.Listen("tcp", address)
		if listener != nil {
			listener.Close()
		}
		if !isAddressInUse(err) {
			t.Fatalf("dual-stack reservation did not hold %s: %v", address, err)
		}
	}
	if err := occupied[1].Close(); err != nil {
		t.Fatal(err)
	}
	occupied = occupied[:1]

	t.Setenv("ASSIGNED_PORTS", "")
	ports, held, err := assignPorts(map[string]svclib.ServiceSpec{
		"service": {AutoassignPort: true, Port: portString, PortFamily: "ipv6", SoReuseportAware: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closePorts(held); err != nil {
			t.Error(err)
		}
	})
	if got := ports["service"]; got != portString {
		t.Fatalf("assigned port %q, want %q", got, portString)
	}
	if err := closePorts(occupied); err != nil {
		t.Fatal(err)
	}
	occupied = nil

	// A dual-stack reservation must fail on IPv6 and release its IPv4 socket.
	colliding, _, err := reserveServicePort("dual-stack", portString, true)
	if err == nil {
		closePorts(colliding)
		t.Fatal("reserved occupied IPv6 port")
	}
	if !isAddressInUse(err) || !strings.Contains(err.Error(), net.JoinHostPort("::1", portString)) {
		t.Fatalf("expected an IPv6 bind failure, got %v", err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", portString))
	if err != nil {
		t.Fatalf("IPv4 reservation leaked: %v", err)
	}
	listener.Close()
}
