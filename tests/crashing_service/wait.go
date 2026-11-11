package main

import "time"

func main() {
	// Keep the test running past the service's one-second crash.
	time.Sleep(2 * time.Second)
}
