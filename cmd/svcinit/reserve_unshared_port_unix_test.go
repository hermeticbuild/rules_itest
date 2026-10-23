//go:build unix

package main

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
)

func TestWildcardListenerOccupiesServicePort(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requireServicePortOccupied(t, listener.Addr().(*net.TCPAddr).Port)
}

func TestBoundSocketOccupiesServicePort(t *testing.T) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	address, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	requireServicePortOccupied(t, address.(*syscall.SockaddrInet4).Port)
}

func requireServicePortOccupied(t *testing.T, port int) {
	t.Helper()
	for _, reusable := range []bool{false, true} {
		reservation, _, err := reserveServicePort("ipv4", strconv.Itoa(port), reusable)
		if closeErr := closePorts(reservation); closeErr != nil {
			t.Error(closeErr)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("reuse-aware=%t: assigning occupied port %d: got %v, want EADDRINUSE", reusable, port, err)
		}
	}
}
