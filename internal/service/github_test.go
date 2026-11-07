package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateAPIURL(t *testing.T) {
	for _, raw := range []string{"https://api.github.com", "http://127.0.0.1:1", "http://localhost:2", "http://[::1]:3"} {
		if _, err := ValidateAPIURL(raw); err != nil {
			t.Errorf("%q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://example.com", "https://u:p@github.com", "https://github.com?x=1", "ftp://x", "not a url"} {
		if _, err := ValidateAPIURL(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestGitHubClient(t *testing.T) {
	var dispatchBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
			_ = json.NewDecoder(r.Body).Decode(&dispatchBody)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs":
			if r.URL.Query().Get("event") != "workflow_dispatch" || r.URL.Query().Get("branch") != "main" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{
				"id": 42, "head_sha": "abc", "status": "in_progress", "html_url": "https://example/42",
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": "completed", "conclusion": "success"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/42/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{{
				"id": 7, "name": "deploy", "status": "completed", "conclusion": "success",
				"steps": []map[string]any{{"number": 1, "name": "Checkout", "status": "completed", "conclusion": "success"}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/jobs/7/logs":
			_, _ = w.Write([]byte("hello log\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/jobs/8/logs":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &GitHubClient{BaseURL: server.URL, Token: "token", HTTPClient: server.Client()}
	ctx := t.Context()

	if err := client.Dispatch(ctx, "o", "r", "deploy.yml", "main", map[string]string{"image_tag": "abc"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if dispatchBody["ref"] != "main" {
		t.Errorf("dispatch body = %v", dispatchBody)
	}
	inputs, _ := dispatchBody["inputs"].(map[string]any)
	if inputs["image_tag"] != "abc" {
		t.Errorf("inputs = %v", inputs)
	}

	runs, err := client.ListRuns(ctx, "o", "r", "main")
	if err != nil || len(runs) != 1 || runs[0].ID != 42 {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	run, err := client.GetRun(ctx, "o", "r", 42)
	if err != nil || run.Conclusion != "success" {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	jobs, err := client.ListJobs(ctx, "o", "r", 42)
	if err != nil || len(jobs) != 1 || len(jobs[0].Steps) != 1 {
		t.Fatalf("jobs = %+v, err = %v", jobs, err)
	}

	logs, ready, err := client.JobLogs(ctx, "o", "r", 7)
	if err != nil || !ready || logs != "hello log\n" {
		t.Errorf("logs = %q ready=%v err=%v", logs, ready, err)
	}
	_, ready, err = client.JobLogs(ctx, "o", "r", 8)
	if err != nil || ready {
		t.Errorf("404 logs ready=%v err=%v", ready, err)
	}
}

func TestGitHubClientErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &GitHubClient{BaseURL: server.URL, Token: "bad", HTTPClient: server.Client()}
	if err := client.Dispatch(t.Context(), "o", "r", "deploy.yml", "main", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v", err)
	}
}
