package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
