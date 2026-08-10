// Package gmgn is the GMGN OpenAPI adapter. It is a data source only — it
// knows nothing about clusters, signals or strategies.
package gmgn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const defaultBase = "https://openapi.gmgn.ai"

// RateLimitError carries the reset time so the collector can sleep instead of
// spamming (repeated 429s extend the ban by 5s each).
type RateLimitError struct {
	ResetAt time.Time // zero if unknown
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("gmgn rate limited (resets %s)", e.ResetAt.Format(time.RFC3339))
}

// Client talks to the GMGN OpenAPI.
type Client struct {
	base   string
	apiKey string
	hc     *http.Client
}

func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBase
	}
	// GMGN rejects IPv6 sources; force IPv4 dialing (skill-documented).
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Client{
		base:   baseURL,
		apiKey: apiKey,
		hc:     &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}
}

// get performs an authenticated GET; out receives the decoded `data` payload.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	q.Set("timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	q.Set("client_id", fmt.Sprintf("followedge-%d", time.Now().UnixNano()))
	u := c.base + path + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-APIKEY", c.apiKey)
	req.Header.Set("User-Agent", "followedge/0.1")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("gmgn request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("gmgn read body: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{ResetAt: parseReset(resp.Header.Get("X-RateLimit-Reset"), body)}
	}

	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("gmgn bad json: %w", err)
	}
	if envelope.Code != 0 {
		if envelope.Code == 429 {
			return &RateLimitError{ResetAt: parseReset("", body)}
		}
		return fmt.Errorf("gmgn error code=%d msg=%s", envelope.Code, envelope.Message)
	}
	if len(envelope.Data) == 0 {
		return nil // some routes may return empty data
	}
	return json.Unmarshal(envelope.Data, out)
}

// parseReset extracts the rate-limit reset from the header (unix seconds) or
// the body's reset_at field.
func parseReset(headerVal string, body []byte) time.Time {
	if headerVal != "" {
		if secs, err := strconv.ParseInt(headerVal, 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0)
		}
	}
	var m struct {
		ResetAt int64 `json:"reset_at"`
	}
	if err := json.Unmarshal(body, &m); err == nil && m.ResetAt > 0 {
		return time.Unix(m.ResetAt, 0)
	}
	return time.Time{}
}

var errNoData = errors.New("gmgn: empty data")
