package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGitHubAPI = "https://api.github.com"
	maxErrorSnippet  = 512
)

// RateLimitError reports a 403/429 response with GitHub's retry hint.
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github rate limited (%d), retry after %s", e.Status, e.RetryAfter)
}

func rateLimitFrom(resp *http.Response) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	wait := 60 * time.Second
	if value := resp.Header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			wait = time.Duration(seconds) * time.Second
		}
	} else if value := resp.Header.Get("X-RateLimit-Reset"); value != "" {
		if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
			if until := time.Until(time.Unix(epoch, 0)); until > 0 {
				wait = until
			} else {
				wait = 0
			}
		}
	}
	return &RateLimitError{Status: resp.StatusCode, RetryAfter: wait}
}

// GitHubClient is a small REST client for workflow dispatch and run data.
type GitHubClient struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// WorkflowRun is one GitHub Actions run.
type WorkflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	HeadSHA    string    `json:"head_sha"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	Path       string    `json:"path"`
	CreatedAt  time.Time `json:"created_at"`
}

// JobStep is one step within a job.
type JobStep struct {
	Number     int    `json:"number"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Job is one workflow job.
type Job struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Steps      []JobStep `json:"steps"`
}

func (c *GitHubClient) baseURL() string {
	if strings.TrimSpace(c.BaseURL) == "" {
		return defaultGitHubAPI
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *GitHubClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *GitHubClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "payload")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	return resp, nil
}

func (c *GitHubClient) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if rateErr := rateLimitFrom(resp); rateErr != nil {
		return rateErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		return fmt.Errorf("github api %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode github response: %w", err)
		}
	}
	return nil
}

// Dispatch triggers a workflow_dispatch run.
func (c *GitHubClient) Dispatch(ctx context.Context, owner, repo, workflow, ref string, inputs map[string]string) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(workflow))
	body := struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{Ref: ref, Inputs: inputs}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// ListRuns lists recent workflow_dispatch runs for a branch.
func (c *GitHubClient) ListRuns(ctx context.Context, owner, repo, branch string) ([]WorkflowRun, error) {
	query := url.Values{}
	query.Set("event", "workflow_dispatch")
	query.Set("branch", branch)
	query.Set("per_page", "20")
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?%s",
		url.PathEscape(owner), url.PathEscape(repo), query.Encode())
	var response struct {
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.WorkflowRuns, nil
}

// GetRun refreshes one run.
func (c *GitHubClient) GetRun(ctx context.Context, owner, repo string, runID int64) (WorkflowRun, error) {
	var run WorkflowRun
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(repo), runID)
	if err := c.do(ctx, http.MethodGet, path, nil, &run); err != nil {
		return run, err
	}
	return run, nil
}

// ListJobs returns the jobs and steps of a run.
func (c *GitHubClient) ListJobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo), runID)
	var response struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Jobs, nil
}

// JobLogs fetches plain-text logs for one job. ready is false while GitHub
// still responds 404/410 (logs are not available before the job starts).
func (c *GitHubClient) JobLogs(ctx context.Context, owner, repo string, jobID int64) (logs string, ready bool, err error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs",
		url.PathEscape(owner), url.PathEscape(repo), jobID)
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if rateErr := rateLimitFrom(resp); rateErr != nil {
		return "", false, rateErr
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		return "", false, fmt.Errorf("github job logs: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, fmt.Errorf("read job logs: %w", err)
	}
	return string(data), true, nil
}

// ValidateAPIURL requires https, except loopback http for tests. It also
// rejects userinfo, query and fragment so a token cannot be redirected.
func ValidateAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("URL must be absolute with a scheme and host")
	}
	if parsed.User != nil {
		return "", errors.New("URL must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("URL must not contain a query or fragment")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("http is only allowed for loopback hosts; use https")
		}
	default:
		return "", errors.New("URL scheme must be https")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}
