// tcp_probe dials the address given as its first argument and exits 0 if a TCP
// connection can be established, or non-zero otherwise. It stands in for a real
// database/network readiness probe used as an itest_external_service command
// health check, for services that expose no HTTP endpoint.
package main

import (
	"log"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: tcp_probe host:port")
	}

	conn, err := net.DialTimeout("tcp", os.Args[1], 2*time.Second)
	if err != nil {
		log.Printf("tcp_probe: %v", err)
		os.Exit(1)
	}
	conn.Close()
}
