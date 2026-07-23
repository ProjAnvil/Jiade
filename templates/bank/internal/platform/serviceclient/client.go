// Package serviceclient provides read-only HTTP calls between microservices.
package serviceclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a JSON HTTP client with timeout.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a service client. The trailing slash at the baseURL will be removed.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

// Get requests path and decodes the successful response to out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("创建服务请求: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("调用 %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("调用 %s 返回 %s: %s", c.baseURL, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析 %s 响应: %w", c.baseURL, err)
	}
	return nil
}

// EscapePath Escapes a single URL path parameter.
func EscapePath(value string) string { return url.PathEscape(value) }
