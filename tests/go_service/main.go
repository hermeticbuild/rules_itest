package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

var fibSink int

func main() {
	sleepTime := flag.Duration("sleep-time", 0, "How long to sleep before binding the port")
	busyWaitTime := flag.Duration("busy-time", 0, "How long to busy-wait before binding the port")
	dieAfter := flag.Duration("die-after", 0, "How long to wait before self-destructing")
	fileToOpen := flag.String("file-to-open", "", "A file to open to check runfiles")
	soReuseport := flag.Bool("so-reuseport", false, "If true, sets SO_REUSEPORT when binding the address")
	port := flag.String("port", "", "Port to bind")

	flag.Parse()

	if *dieAfter != 0 {
		go func() {
			<-time.After(*dieAfter)
			os.Exit(1)
		}()
	}

	if *port == "" {
		portStr := os.Getenv("PORT")
		port = &portStr
	}

	if *fileToOpen != "" {
		resolvedPath, err := runfiles.Rlocation(*fileToOpen)
		if err != nil {
			panic(err)
		}
		f, err := os.Open(resolvedPath)
		if err != nil {
			panic(err)
		}
		f.Close()
	}

	log.Println("started")
	time.Sleep(*sleepTime)
	log.Println("done sleeping")

	finishBusyWait := time.Now().Add(*busyWaitTime)
	for time.Now().Before(finishBusyWait) {
		fibSink += fib(10)
	}

	dob := time.Now()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	http.HandleFunc("/dob", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(dob.String()))
	})
	http.HandleFunc("/fib", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strconv.Itoa(fibSink)))
	})

	go func() {
		for {
			time.Sleep(100 * time.Millisecond)
			log.Print("HIII STILL ALIVE")
		}
	}()

	serve(*port, *soReuseport)

}

func fib(n int) int {
	if n < 2 {
		return 1
	}
	return fib(n-1) + fib(n-2)
}
