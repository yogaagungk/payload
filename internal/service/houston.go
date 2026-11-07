package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ReleaseApprovalRequest is the payload webhook body.
type ReleaseApprovalRequest struct {
	Repo        string            `json:"repo"`
	Ref         string            `json:"ref"`
	SHA         string            `json:"sha"`
	Workflow    string            `json:"workflow"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	RequestedBy string            `json:"requested_by,omitempty"`
	Message     string            `json:"message,omitempty"`
}

// HoustonRelease is a release-approval request from houston.
type HoustonRelease struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	Workflow  string    `json:"workflow"`
	Status    string    `json:"status"`
	RunID     *int64    `json:"run_id"`
	RunURL    *string   `json:"run_url"`
	Error     *string   `json:"error"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HoustonError carries the HTTP status and any retry hint.
type HoustonError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *HoustonError) Error() string {
	return fmt.Sprintf("houston api %d: %s", e.Status, e.Message)
}

// HoustonClient talks to the release-approval webhook.
type HoustonClient struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func (c *HoustonClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *HoustonClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("houston request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		wait := time.Duration(0)
		if seconds := resp.Header.Get("Retry-After"); seconds != "" {
			if parsed, err := strconv.Atoi(seconds); err == nil {
				wait = time.Duration(parsed) * time.Second
			}
		}
		return resp.StatusCode, &HoustonError{Status: resp.StatusCode, Message: strings.TrimSpace(string(snippet)), RetryAfter: wait}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode houston response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// CreateRelease queues a release-approval request.
func (c *HoustonClient) CreateRelease(ctx context.Context, req ReleaseApprovalRequest) (HoustonRelease, bool, error) {
	var release HoustonRelease
	status, err := c.do(ctx, http.MethodPost, "/api/v1/releases", req, &release)
	return release, status == http.StatusCreated, err
}

// GetRelease polls one release.
func (c *HoustonClient) GetRelease(ctx context.Context, id string) (HoustonRelease, error) {
	var release HoustonRelease
	_, err := c.do(ctx, http.MethodGet, "/api/v1/releases/"+id, nil, &release)
	return release, err
}
