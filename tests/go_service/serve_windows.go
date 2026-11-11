//go:build windows

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"syscall"

	"golang.org/x/sys/windows"
)

func serve(port string, soReuseport bool) {
	lc := net.ListenConfig{
		Control: func(network, address string, conn syscall.RawConn) error {
			if !soReuseport {
				return nil
			}

			var setSockoptErr error
			err := conn.Control(func(fd uintptr) {
				// FIX: Cast fd directly to syscall.Handle instead of int
				setSockoptErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, windows.SO_REUSEADDR, 1)
			})
			if err != nil {
				return err
			}
			return setSockoptErr
		},
	}

	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		log.Fatalf("listen with -so-reuseport=%t: %v", soReuseport, err)
	}
	http.Serve(l, nil)
}
