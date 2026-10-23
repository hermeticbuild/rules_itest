package test_args

import (
	"flag"
	"os"
	"testing"
)

var (
	testArg   = flag.String("test-arg", "", "a literal test argument")
	testPort  = flag.String("test-port", "", "a port-substituted test argument")
	namedPort = flag.String("named-port", "", "the named port re-exported by the outer group")
)

func TestArgs(t *testing.T) {
	if *testArg != "hello" {
		t.Fatalf("expected test-arg=hello, got: %q (os.Args: %v)", *testArg, os.Args)
	}
}

func TestPortArg(t *testing.T) {
	port := *testPort
	if port == "" || port[0] == '$' {
		t.Fatalf("port was not substituted, got: %q (os.Args: %v)", *testPort, os.Args)
	}
	if port != *namedPort {
		t.Fatalf("outer group port %q differs from named port %q", port, *namedPort)
	}
}
