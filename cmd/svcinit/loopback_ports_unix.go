//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type socketHandle = int

// SO_REUSEADDR would let an unaware Linux listener claim a bind-only reservation.
const reusablePortOption = unix.SO_REUSEPORT

func isAddressInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func openPortSocket(family int) (socketHandle, io.Closer, error) {
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, nil, err
	}
	syscall.CloseOnExec(fd)
	return fd, os.NewFile(uintptr(fd), "rules_itest_reserved_port"), nil
}
