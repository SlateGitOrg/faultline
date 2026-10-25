package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SlateGitOrg/faultline/internal/engine"
	"github.com/SlateGitOrg/faultline/internal/fault"
)

// APIError is a non-2xx response from the control API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control API: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// IsNotFound reports whether err is a 404 from the control API.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Client talks to a faultline control API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for base ("127.0.0.1:7070" or a full URL).
func NewClient(base, token string) *Client {
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Base returns the API base URL.
func (c *Client) Base() string { return c.base }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("control API %s %s: read: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		var eb errorBody
		if json.Unmarshal(data, &eb) != nil || eb.Error == "" {
			eb.Error = strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Message: eb.Error}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("control API %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// Health fetches the health endpoint.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
	return h, err
}

// Proxies lists proxies.
func (c *Client) Proxies(ctx context.Context) ([]engine.ProxyInfo, error) {
	var out []engine.ProxyInfo
	err := c.do(ctx, http.MethodGet, "/v1/proxies", nil, &out)
	return out, err
}

// Proxy describes one proxy.
func (c *Client) Proxy(ctx context.Context, name string) (engine.ProxyInfo, error) {
	var out engine.ProxyInfo
	err := c.do(ctx, http.MethodGet, "/v1/proxies/"+url.PathEscape(name), nil, &out)
	return out, err
}

// CreateProxy creates a proxy.
func (c *Client) CreateProxy(ctx context.Context, spec engine.ProxySpec) (engine.ProxyInfo, error) {
	var out engine.ProxyInfo
	err := c.do(ctx, http.MethodPost, "/v1/proxies", spec, &out)
	return out, err
}

// DeleteProxy deletes a proxy.
func (c *Client) DeleteProxy(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/proxies/"+url.PathEscape(name), nil, nil)
}

// AddFault installs a fault on a proxy.
func (c *Client) AddFault(ctx context.Context, proxy string, spec fault.Spec) (fault.Info, error) {
	var out fault.Info
	err := c.do(ctx, http.MethodPost, "/v1/proxies/"+url.PathEscape(proxy)+"/faults", spec, &out)
	return out, err
}

// Faults lists a proxy's faults.
func (c *Client) Faults(ctx context.Context, proxy string) ([]fault.Info, error) {
	var out []fault.Info
	err := c.do(ctx, http.MethodGet, "/v1/proxies/"+url.PathEscape(proxy)+"/faults", nil, &out)
	return out, err
}

// RemoveFault uninstalls one fault.
func (c *Client) RemoveFault(ctx context.Context, proxy, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/proxies/"+url.PathEscape(proxy)+"/faults/"+url.PathEscape(id), nil, nil)
}

// ClearFaults uninstalls every fault on a proxy.
func (c *Client) ClearFaults(ctx context.Context, proxy string) (int, error) {
	var out Removed
	err := c.do(ctx, http.MethodDelete, "/v1/proxies/"+url.PathEscape(proxy)+"/faults", nil, &out)
	return out.Removed, err
}

// ClearAll uninstalls every fault on every proxy.
func (c *Client) ClearAll(ctx context.Context) (int, error) {
	var out Removed
	err := c.do(ctx, http.MethodDelete, "/v1/faults", nil, &out)
	return out.Removed, err
}

// KillSwitch reads the kill switch.
func (c *Client) KillSwitch(ctx context.Context) (engine.KillState, error) {
	var out engine.KillState
	err := c.do(ctx, http.MethodGet, "/v1/killswitch", nil, &out)
	return out, err
}

// SetKillSwitch engages or releases the kill switch.
func (c *Client) SetKillSwitch(ctx context.Context, engaged bool, reason string) (engine.KillState, error) {
	var out engine.KillState
	err := c.do(ctx, http.MethodPost, "/v1/killswitch", KillSwitchRequest{Engaged: engaged, Reason: reason}, &out)
	return out, err
}

// Audit returns audit events with sequence numbers greater than since.
func (c *Client) Audit(ctx context.Context, since int64) ([]engine.Event, error) {
	var out []engine.Event
	err := c.do(ctx, http.MethodGet, "/v1/audit?since="+strconv.FormatInt(since, 10), nil, &out)
	return out, err
}
