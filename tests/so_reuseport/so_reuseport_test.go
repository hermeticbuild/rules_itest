package so_reuseport

import (
	"os"
	"os/exec"
	"testing"

	"net"
)

func TestNo_SO_REUSEPORT(t *testing.T) {
	portNames := []string{
		"@@//so_reuseport:reuseport_service",
		"@@//so_reuseport:reuseport_service.named_port1",
	}

	t.Logf("ASSIGNED_PORTS: %v", os.Getenv("ASSIGNED_PORTS"))

	for _, portName := range portNames {
		t.Logf("Testing port: %s", portName)
		cmd := exec.Command(os.Getenv("GET_ASSIGNED_PORT_BIN"), portName)
		port, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Failed to get port: %v", err)
		}

		if len(port) == 0 {
			t.Fatalf("Cannot get port")
		}
		t.Logf("Port assigned: %s", port)

		// Should fail because we didn't set SO_REUSEPORT
		_, err = net.Listen("tcp", "127.0.0.1:"+string(port))
		if err == nil {
			t.Fatalf("Expected error, got nil")
		}
		t.Logf("Expected error: %v", err)
	}
}
