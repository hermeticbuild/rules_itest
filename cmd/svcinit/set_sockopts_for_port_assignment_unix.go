//go:build unix

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func setSockoptsForPortAssignment(fd uintptr, reusable bool) error {
	if err := syscall.SetsockoptLinger(int(fd), syscall.SOL_SOCKET, syscall.SO_LINGER, &syscall.Linger{Onoff: 1}); err != nil {
		return err
	}
	if !reusable {
		// Go enables SO_REUSEADDR before ListenConfig.Control. Disable it so
		// the availability probe cannot share a wildcard or bound-only socket.
		return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
	}

	// It's unfortunate that we need `unix` here; SO_REUSEPORT is defined on linuxarm64 but not linux...
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
}
