// Package typesafe is a minimal HTTP client for TypeSafe's System One API
// (https://docs.typesafe.ai/api.md). There is no Go SDK, so this wraps a
// single endpoint: POST /v1/systemone, which answers a batch of typed
// questions ("noul", "choice", "score") about one piece of state in a
// single request. See docs/design/thread-watch.md, "Where Jev fits".
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL    = "https://api.typesafe.ai"
	systemOnePath     = "/v1/systemone"
	defaultTimeout    = 10 * time.Second
	defaultMaxRetries = 3
	maxErrorBodyBytes = 500
	maxResponseBytes  = 1 << 20 // 1 MiB: generous for a judgment response, bounds a pathological reply
)

// Client calls TypeSafe's System One API. The zero value is not usable
// directly for Ask (APIKey is required); build one with NewFromEnv or set
// the fields explicitly. BaseURL, HTTP, and MaxRetries default when left
// zero.
type Client struct {
	BaseURL    string       // default "https://api.typesafe.ai"
	APIKey     string       // required
	HTTP       *http.Client // default: 10s timeout
	MaxRetries int          // default 3; retries beyond the first attempt
}

// NewFromEnv builds a Client from the TYPESAFE_API_KEY environment
// variable. The second return value is false when the variable is unset or
// empty, so callers can fall back to deterministic-only behaviour (per the
// design doc: "If TypeSafe is unavailable, detectors fall back to
// deterministic-only behaviour and never alert less because Jev is down")
// instead of treating a missing key as a hard error.
func NewFromEnv() (*Client, bool) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		return nil, false
	}
	return &Client{APIKey: key}, true
}

// Question is one typed judgment request. Type is "noul", "choice", or
// "score". Instructions is normally a string, but any JSON-marshalable
// value is accepted (the API takes "Question text/structure"). Criteria
// depends on Type: for "noul" an optional {true, false} description map;
// for "choice" a required option-name -> description map (max 255
// options); for "score" a required ordered []string of level descriptions
// (2-10 levels).
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is one typed judgment result. Only the fields relevant to the
// question's Type are populated by the API.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// Usage reports token accounting for one Ask call.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type systemOneRequest struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

type systemOneResponse struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// apiError is returned by Ask for a non-2xx response that either is not
// retryable or exhausted retries. It carries at most a capped, server-side
// error body: never the client's own API key, which is only ever placed in
// the outgoing Authorization header.
type apiError struct {
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("typesafe: http %d: %s", e.StatusCode, e.Body)
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == 529
}

// Ask sends state and all questions in a single POST /v1/systemone request
// — TypeSafe answers independent questions in parallel, so batching keeps
// latency and cost down (see docs/design/thread-watch.md: "All questions
// for one event go in one request"). It retries on HTTP 429, HTTP 529, and
// network errors with jittered exponential backoff, honoring ctx's
// deadline/cancellation between attempts. Other HTTP errors (401, 422, ...)
// are returned immediately without retrying.
func (c *Client) Ask(ctx context.Context, state any, model string, questions map[string]Question) (map[string]Answer, Usage, error) {
	if c.APIKey == "" {
		return nil, Usage{}, errors.New("typesafe: no API key configured")
	}

	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	maxRetries := c.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}

	reqBody, err := json.Marshal(systemOneRequest{State: state, Model: model, Questions: questions})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("typesafe: encoding request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, backoff(attempt)); err != nil {
				return nil, Usage{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, Usage{}, err
		}

		answers, usage, retryable, err := c.doOnce(ctx, httpClient, baseURL, reqBody)
		if err == nil {
			return answers, usage, nil
		}
		if ctx.Err() != nil {
			return nil, Usage{}, ctx.Err()
		}
		lastErr = err
		if !retryable {
			return nil, Usage{}, lastErr
		}
	}
	return nil, Usage{}, lastErr
}

// doOnce performs a single HTTP round trip. retryable is true when the
// caller should back off and try again (a network error, 429, or 529).
func (c *Client) doOnce(ctx context.Context, httpClient *http.Client, baseURL string, reqBody []byte) (answers map[string]Answer, usage Usage, retryable bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+systemOnePath, bytes.NewReader(reqBody))
	if err != nil {
		return nil, Usage{}, false, fmt.Errorf("typesafe: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, Usage{}, true, fmt.Errorf("typesafe: request failed: %w", scrubKey(err, c.APIKey))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, Usage{}, true, fmt.Errorf("typesafe: reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &apiError{StatusCode: resp.StatusCode, Body: truncate(strings.TrimSpace(string(body)), maxErrorBodyBytes)}
		return nil, Usage{}, isRetryableStatus(resp.StatusCode), apiErr
	}

	var parsed systemOneResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, Usage{}, false, fmt.Errorf("typesafe: decoding response: %w", err)
	}
	return parsed.Answers, parsed.Usage, false, nil
}

// backoff returns a jittered exponential delay for the given attempt
// (1-based: the first retry). It is capped so a long run of retries still
// yields regularly to ctx.
func backoff(attempt int) time.Duration {
	const (
		base     = 200 * time.Millisecond
		maxDelay = 5 * time.Second
	)
	d := base
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	// Half fixed, half jitter, so concurrent retries don't all land at once.
	jitter := time.Duration(rand.Int63n(int64(d)/2 + 1))
	return d/2 + jitter
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// scrubKey defensively strips the API key from a wrapped error's text.
// Ordinary transport errors (DNS, timeout, connection refused) only ever
// echo the request URL, which never contains the key — the key is sent
// solely in the Authorization header — but this keeps that guarantee even
// if a future net/http error type changes what it stringifies.
func scrubKey(err error, key string) error {
	if key == "" || err == nil {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, key) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, key, "[redacted]"))
}
