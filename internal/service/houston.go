package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (c *HoustonClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("houston request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("houston api %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode houston response: %w", err)
		}
	}
	return nil
}

// CreateRelease queues a release-approval request.
func (c *HoustonClient) CreateRelease(ctx context.Context, req ReleaseApprovalRequest) (HoustonRelease, error) {
	var release HoustonRelease
	err := c.do(ctx, http.MethodPost, "/api/v1/releases", req, &release)
	return release, err
}

// GetRelease polls one release.
func (c *HoustonClient) GetRelease(ctx context.Context, id string) (HoustonRelease, error) {
	var release HoustonRelease
	err := c.do(ctx, http.MethodGet, "/api/v1/releases/"+id, nil, &release)
	return release, err
}
