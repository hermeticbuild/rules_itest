package main

import (
	"log"
	"net"
	"os"
	"time"
)

// Run as a prerequisite of the service that owns these ports.
func main() {
	for _, port := range os.Args[1:] {
		addr, err := net.ResolveTCPAddr("tcp4", "127.0.0.1:"+port)
		if err != nil || addr.Port == 0 {
			log.Fatalf("invalid assigned port %q: %v", port, err)
		}
		if listener, err := net.ListenTCP("tcp4", addr); err == nil {
			listener.Close()
			log.Fatalf("prerequisite claimed %s", addr)
		}
		if conn, err := net.DialTimeout("tcp4", addr.String(), time.Second); err == nil {
			conn.Close()
			log.Fatalf("connected to %s before its service started", addr)
		}
	}
}
