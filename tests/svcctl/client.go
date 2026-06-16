package svcctl

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
)

// BindingInfo mirrors svclib.BindingInfo for the entries returned by /v0/ports and /v0/services.
type BindingInfo struct {
	Origin string `json:"origin"`
	Domain string `json:"domain"`
	Port   string `json:"port"`
}

type SvcctlClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewSvcctlClient(baseURL string, client *http.Client) *SvcctlClient {
	return &SvcctlClient{
		baseURL:    baseURL,
		httpClient: client,
	}
}

func (c *SvcctlClient) StartService(ctx context.Context, service string, waitForHealthy bool) error {
	q := url.Values{}
	q.Set("service", service)
	if waitForHealthy {
		q.Set("wait_for_healthy", "1")
	} else {
		q.Set("wait_for_healthy", "0")
	}

	log.Printf(c.baseURL+"/v0/start?" + q.Encode())

	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/v0/start?" + q.Encode(), nil)
	if err != nil {
		return err
	}

	req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to start speedy service: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Got status code %d, want %d", resp.StatusCode, http.StatusOK)
	}

	return nil
}

func (c *SvcctlClient) WaitForService(ctx context.Context, service string) error {
	q := url.Values{}
	q.Set("service", service)

	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/v0/wait?" + q.Encode(), nil)
	if err != nil {
		return err
	}

	req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to start speedy service: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Got status code %d, want %d", resp.StatusCode, http.StatusOK)
	}

	return nil
}

func (c *SvcctlClient) HealthCheck(ctx context.Context, service string) (int, error) {
	q := url.Values{}
	q.Set("service", service)

	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/v0/healthcheck?" + q.Encode(), nil)
	if err != nil {
		return -1, err
	}

	req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return -1, err
	}

	return resp.StatusCode, nil
}

// Ports fetches the full ITEST_PORTS_MAP via /v0/ports.
func (c *SvcctlClient) Ports(ctx context.Context) (map[string]BindingInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v0/ports", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Got status code %d, want %d", resp.StatusCode, http.StatusOK)
	}

	out := map[string]BindingInfo{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Services fetches the full ITEST_SERVICES_MAP via /v0/services.
func (c *SvcctlClient) Services(ctx context.Context) (map[string]map[string]BindingInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v0/services", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Got status code %d, want %d", resp.StatusCode, http.StatusOK)
	}

	out := map[string]map[string]BindingInfo{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}