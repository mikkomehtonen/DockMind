package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	url    string
	client *http.Client
}

type runningResponse struct {
	Running []struct {
		Model string `json:"model"`
	} `json:"running"`
}

func New(url string) *Client {
	return &Client{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Check(ctx context.Context) (bool, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return false, nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return false, nil, nil
	}

	var result runningResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, nil, err
	}

	var models []string
	for _, r := range result.Running {
		if r.Model != "" {
			models = append(models, r.Model)
		}
	}
	return true, models, nil
}

// UnloadClient calls the llama-swap unload endpoint to free GPU VRAM by
// unloading all currently loaded models. The endpoint is GET <backendURL>/unload.
type UnloadClient struct {
	url    string
	client *http.Client
}

// NewUnloadClient constructs an UnloadClient targeting <backendURL>/unload.
// Trailing slashes on backendURL are trimmed so the resulting path is /unload
// (not //unload).
func NewUnloadClient(backendURL string) *UnloadClient {
	return &UnloadClient{
		url:    strings.TrimRight(backendURL, "/") + "/unload",
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Unload issues GET <backendURL>/unload. It returns nil on a 200 OK response
// and an error on any non-200 status or transport failure.
func (c *UnloadClient) Unload(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama-swap unload returned status %d", resp.StatusCode)
	}
	return nil
}
