//go:build unix

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
)

func reservePort(port int, reusable bool) (io.Closer, string, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return nil, "", fmt.Errorf("socket: %w", err)
	}
	syscall.CloseOnExec(fd)

	file := os.NewFile(uintptr(fd), "rules_itest_reserved_port")
	success := false
	defer func() {
		if !success {
			file.Close()
		}
	}()

	if reusable {
		// Do not set SO_REUSEADDR here. Go TCP listeners enable it by default on
		// Linux, where it would allow an unaware listener to claim a bind-only
		// reservation. SO_REUSEPORT alone allows the aware service to share it.
		if err := setSockoptsForPortAssignment(uintptr(fd), &syscall.Linger{
			Onoff:  1,
			Linger: 0,
		}); err != nil {
			return nil, "", fmt.Errorf("set reusable reservation socket options: %w", err)
		}
	}

	if err := syscall.Bind(fd, &syscall.SockaddrInet4{
		Port: port,
		Addr: [4]byte{127, 0, 0, 1},
	}); err != nil {
		return nil, "", fmt.Errorf("bind reservation socket: %w", err)
	}

	addr, err := syscall.Getsockname(fd)
	if err != nil {
		return nil, "", fmt.Errorf("getsockname reservation socket: %w", err)
	}
	tcpAddr, ok := addr.(*syscall.SockaddrInet4)
	if !ok {
		return nil, "", fmt.Errorf("getsockname returned %T, expected *syscall.SockaddrInet4", addr)
	}

	success = true
	return file, strconv.Itoa(tcpAddr.Port), nil
}
