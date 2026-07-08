package health

import (
	"context"
	"io"
	"net/http"
	"time"
)

type Client struct {
	url    string
	client *http.Client
}

func New(url string) *Client {
	return &Client{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Check(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK, nil
}
