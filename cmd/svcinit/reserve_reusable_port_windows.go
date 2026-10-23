//go:build windows

package main

import (
	"fmt"
	"io"
	"strconv"

	"golang.org/x/sys/windows"
)

type windowsPortReservation struct {
	socket windows.Handle
}

func (r *windowsPortReservation) Close() error {
	return windows.Closesocket(r.socket)
}

func reserveReusablePort(port int) (io.Closer, string, error) {
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

	if err := setSockoptsForPortAssignment(uintptr(socket), true); err != nil {
		return nil, "", fmt.Errorf("set reusable reservation socket options: %w", err)
	}

	if err := windows.Bind(socket, &windows.SockaddrInet4{
		Port: port,
		Addr: [4]byte{127, 0, 0, 1},
	}); err != nil {
		return nil, "", fmt.Errorf("bind reusable reservation socket: %w", err)
	}

	addr, err := windows.Getsockname(socket)
	if err != nil {
		return nil, "", fmt.Errorf("getsockname reusable reservation socket: %w", err)
	}
	tcpAddr, ok := addr.(*windows.SockaddrInet4)
	if !ok {
		return nil, "", fmt.Errorf("getsockname returned %T, expected *windows.SockaddrInet4", addr)
	}

	success = true
	return &windowsPortReservation{socket: socket}, strconv.Itoa(tcpAddr.Port), nil
}
