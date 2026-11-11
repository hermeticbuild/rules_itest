package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func main() {
	log.SetFlags(0)
	r, err := runfiles.New()
	if err != nil {
		log.Fatal(err)
	}
	test, err := r.Rlocation(os.Getenv("TEST_TO_RUN"))
	if err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(test)
	cmd.Env = append(os.Environ(), r.Env()...)
	output, err := cmd.CombinedOutput()
	os.Stdout.Write(output)
	if err == nil {
		log.Fatalf("expected %s to fail", test)
	}
	if _, ok := err.(*exec.ExitError); !ok {
		log.Fatal(err)
	}
	if want := os.Getenv("EXPECTED_FAILURE"); !bytes.Contains(output, []byte(want)) {
		log.Fatalf("%s: expected failure containing %q", err, want)
	}
}
