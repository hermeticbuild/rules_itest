//go:build windows

package main

import "syscall"

func setSockoptsForPortAssignment(fd uintptr, reusable bool) error {
	if err := syscall.SetsockoptLinger(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_LINGER, &syscall.Linger{Onoff: 1}); err != nil {
		return err
	}
	if !reusable {
		// Go does not enable address reuse on Windows listeners.
		return nil
	}

	// Windows has no SO_REUSEPORT. SO_REUSEADDR allows the service listener to
	// bind while svcinit holds a bind-only reservation socket.
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
