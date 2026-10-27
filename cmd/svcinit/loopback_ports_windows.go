//go:build windows

package main

import (
	"errors"
	"io"
	"syscall"

	"golang.org/x/sys/windows"
)

type socketHandle = syscall.Handle

// Winsock uses SO_REUSEADDR to share the service manager's bind-only socket.
const reusablePortOption = syscall.SO_REUSEADDR

type windowsPortReservation windows.Handle

func (r windowsPortReservation) Close() error {
	return windows.Closesocket(windows.Handle(r))
}

func isAddressInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

func openPortSocket(family int) (socketHandle, io.Closer, error) {
	socket, err := windows.WSASocket(
		int32(family),
		windows.SOCK_STREAM,
		windows.IPPROTO_TCP,
		nil,
		0,
		windows.WSA_FLAG_OVERLAPPED|windows.WSA_FLAG_NO_HANDLE_INHERIT,
	)
	if err != nil {
		return 0, nil, err
	}
	return socketHandle(socket), windowsPortReservation(socket), nil
}
