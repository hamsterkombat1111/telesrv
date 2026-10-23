package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	MaxProviderRequest  = 4 << 20
	MaxProviderResponse = 16 << 20
)

var (
	ErrProviderRequest     = errors.New("invalid wallet provider request")
	ErrProviderResponse    = errors.New("invalid wallet provider response")
	ErrProviderUnavailable = errors.New("wallet provider unavailable")
)

// ProviderHTTPError carries only the upstream status. URLs, credentials and
// bodies are intentionally excluded from errors that may be logged by RPC.
type ProviderHTTPError struct{ Status int }

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("wallet provider HTTP status %d", e.Status)
}

// Toncenter implements the transport of toncenter.performApiRequest. It never
// retries: the same endpoint can submit a signed transfer as well as read data.
type Toncenter struct {
	client  *http.Client
	apiKey  string
	baseURL string
}

func NewToncenter(apiKey string, baseURL ...string) *Toncenter {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxConnsPerHost = 16
	transport.MaxIdleConnsPerHost = 16
	transport.ResponseHeaderTimeout = 15 * time.Second
	var base string
	if len(baseURL) > 0 && strings.TrimSpace(baseURL[0]) != "" {
		base = strings.TrimRight(strings.TrimSpace(baseURL[0]), "/")
	}
	return &Toncenter{client: &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, apiKey: apiKey, baseURL: base}
}

// Perform accepts a path, never a caller-selected origin. A nil payload selects
// GET; a present payload selects POST, preserving the TL optional distinction.
func (p *Toncenter) Perform(ctx context.Context, endpoint string, query, payload *string) (string, error) {
	if p == nil || p.client == nil {
		return "", ErrProviderUnavailable
	}

	// If endpoint contains query parameters, split them gracefully.
	if idx := strings.IndexByte(endpoint, '?'); idx >= 0 {
		epQuery := endpoint[idx+1:]
		endpoint = endpoint[:idx]
		if query == nil || *query == "" {
			query = &epQuery
		}
	}
	endpoint = path.Clean(endpoint)
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	if len(endpoint) > 2048 {
		return "", ErrProviderRequest
	}

	var u url.URL
	if p.baseURL != "" {
		parsed, err := url.Parse(p.baseURL)
		if err != nil {
			return "", ErrProviderUnavailable
		}
		u = *parsed
		u.Path = path.Join(u.Path, endpoint)
	} else {
		u = url.URL{Scheme: "https", Host: "toncenter.com", Path: endpoint}
	}

	if query != nil && *query != "" {
		if len(*query) > 64<<10 {
			return "", ErrProviderRequest
		}
		u.RawQuery = *query
	}

	method := http.MethodGet
	var body io.Reader
	contentType := "application/json"
	if payload != nil {
		if len(*payload) > MaxProviderRequest {
			return "", ErrProviderRequest
		}
		method = http.MethodPost
		body = strings.NewReader(*payload)
		if !json.Valid([]byte(*payload)) {
			contentType = "application/x-www-form-urlencoded"
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return "", ErrProviderRequest
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if p.apiKey != "" {
		req.Header.Set("X-API-Key", p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrProviderUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &ProviderHTTPError{Status: resp.StatusCode}
	}
	if resp.ContentLength > MaxProviderResponse {
		return "", ErrProviderResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxProviderResponse+1))
	if err != nil || len(data) > MaxProviderResponse {
		return "", ErrProviderResponse
	}
	return string(data), nil
}
