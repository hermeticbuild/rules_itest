//go:build unix

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"syscall"

	"golang.org/x/sys/unix"
)

func serve(port string, soReuseport bool) {
	lc := net.ListenConfig{
		Control: func(network, address string, conn syscall.RawConn) error {
			if !soReuseport {
				return nil
			}

			var setSockoptErr error
			err := conn.Control(func(fd uintptr) {
				setSockoptErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
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
