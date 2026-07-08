package shelly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	address string
	channel int
	client  *http.Client
}

func New(address string, channel int) *Client {
	return &Client{
		address: address,
		channel: channel,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) SetPower(ctx context.Context, on bool) error {
	u := url.URL{
		Scheme: "http",
		Host:   c.address,
		Path:   "/rpc/Switch.Set",
	}
	q := u.Query()
	q.Set("id", strconv.Itoa(c.channel))
	q.Set("on", strconv.FormatBool(on))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("shelly SetPower returned status %d: %s", resp.StatusCode, body)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) IsOn(ctx context.Context) (bool, error) {
	u := url.URL{
		Scheme: "http",
		Host:   c.address,
		Path:   "/rpc/Switch.GetStatus",
	}
	q := u.Query()
	q.Set("id", strconv.Itoa(c.channel))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("shelly GetStatus returned status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Output bool `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	io.Copy(io.Discard, resp.Body)
	return result.Output, nil
}
