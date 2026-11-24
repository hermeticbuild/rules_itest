//go:build windows

package main

import (
	"fmt"
	"io"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

type windowsPortReservation struct {
	socket windows.Handle
}

func (r *windowsPortReservation) Close() error {
	return windows.Closesocket(r.socket)
}

func reservePort(port int, reusable bool) (io.Closer, string, error) {
	socket, err := windows.WSASocket(
		windows.AF_INET,
		windows.SOCK_STREAM,
		windows.IPPROTO_TCP,
		nil,
		0,
		windows.WSA_FLAG_OVERLAPPED|windows.WSA_FLAG_NO_HANDLE_INHERIT,
	)
	if err != nil {
		return nil, "", fmt.Errorf("socket: %w", err)
	}

	success := false
	defer func() {
		if !success {
			windows.Closesocket(socket)
		}
	}()

	if reusable {
		if err := setSockoptsForPortAssignment(uintptr(socket), &syscall.Linger{
			Onoff:  1,
			Linger: 0,
		}); err != nil {
			return nil, "", fmt.Errorf("set reusable reservation socket options: %w", err)
		}
	} else {
		// Winsock defines SO_EXCLUSIVEADDRUSE as the complement of SO_REUSEADDR.
		if err := windows.SetsockoptInt(socket, windows.SOL_SOCKET, ^windows.SO_REUSEADDR, 1); err != nil {
			return nil, "", fmt.Errorf("set exclusive reservation socket option: %w", err)
		}
	}

	if err := windows.Bind(socket, &windows.SockaddrInet4{
		Port: port,
		Addr: [4]byte{127, 0, 0, 1},
	}); err != nil {
		return nil, "", fmt.Errorf("bind reservation socket: %w", err)
	}

	addr, err := windows.Getsockname(socket)
	if err != nil {
		return nil, "", fmt.Errorf("getsockname reservation socket: %w", err)
	}
	tcpAddr, ok := addr.(*windows.SockaddrInet4)
	if !ok {
		return nil, "", fmt.Errorf("getsockname returned %T, expected *windows.SockaddrInet4", addr)
	}

	success = true
	return &windowsPortReservation{socket: socket}, strconv.Itoa(tcpAddr.Port), nil
}
