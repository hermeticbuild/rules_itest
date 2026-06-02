package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hermeticbuild/rules_itest/tests/svcctl"
)

type payload struct {
	Value string `json:"value"`
}

func getPort(service string) (*string, error) {
	cmd := exec.Command(os.Getenv("GET_ASSIGNED_PORT_BIN"), service)
	port, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Failed to get port: %v", err)
	}
	var portStr = string(port)
	return &portStr, nil
}

func TestStartDeferredService(t *testing.T) {
	port := os.Getenv("SVCCTL_PORT")
	if port == "" {
		t.Errorf("SVCCTL_PORT not set")
	}

	client := svcctl.NewSvcctlClient("http://localhost:" + port, http.DefaultClient)

	log.Println("Starting deferred service...")
	err := client.StartService(context.Background(), "@@//deferred:deferred_itest_service", true)
	if err != nil {
		t.Errorf("Failed to start deferred service: %v", err)
	}

	for {
		code, err := client.HealthCheck(context.Background(), "@@//deferred:deferred_itest_service")

		if err != nil {
			t.Errorf("Failed to health check deferred service: %v", err)
		}
		if code == http.StatusOK {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Println("Getting port for deferred service...")
	servicePort, err := getPort("@@//deferred:deferred_itest_service")
	if err != nil {
		t.Errorf("Failed to get port for deferred service: %v", err)
	}

	v := &payload{
		Value: "test",
	}

	data, _ := json.Marshal(v)

	log.Printf("Deferred service is running on port %s", *servicePort)
	_, err = http.Post("http://localhost:" + *servicePort + "/update", "text/plain", bytes.NewReader(data))
	if err != nil {
		t.Errorf("Failed to call update endpoint: %v", err)
		return
	}

	log.Println("Calling /value endpoint...")
	resp, err := http.Get("http://localhost:" + *servicePort + "/value")
	if err != nil {
		t.Errorf("Failed to call update endpoint: %v", err)
		return
	}

	bodyContent, err := io.ReadAll(resp.Body)

	var result payload
	err = json.Unmarshal(bodyContent, &result)
	if err != nil {
		t.Errorf("Failed to read response body: %v", err)
	}

	if result.Value != "test" {
		t.Errorf("Got response %q, want %q", string(data), "test")
	}

}
