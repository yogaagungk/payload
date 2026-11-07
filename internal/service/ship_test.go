package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingReporter struct {
	mu      sync.Mutex
	started bool
	updates int
	chunks  []string
}

func (r *recordingReporter) RunStarted(WorkflowRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = true
}

func (r *recordingReporter) Update(WorkflowRun, []Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates++
}

func (r *recordingReporter) Log(_ string, chunk string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, chunk)
}

func (r *recordingReporter) logs() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.chunks, "")
}

type shipServer struct {
	mu          sync.Mutex
	completeAt  int
	conclusion  string
	getRunCalls int
	logCalls    int
	dispatched  map[string]any
	headSHA     string
	created     time.Time
}

func newShipServer(t *testing.T, conclusion string, completeAt int) (*shipServer, *httptest.Server) {
	t.Helper()
	state := &shipServer{completeAt: completeAt, conclusion: conclusion, headSHA: "abc123", created: time.Now().UTC()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
			_ = json.NewDecoder(r.Body).Decode(&state.dispatched)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{
				"id": 42, "head_sha": state.headSHA, "status": "queued",
				"html_url": "https://example.test/run/42", "path": ".github/workflows/deploy.yml", "created_at": state.created,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/42":
			state.getRunCalls++
			status, conclusion := "in_progress", ""
			if state.getRunCalls >= state.completeAt {
				status, conclusion = "completed", state.conclusion
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 42, "status": status, "conclusion": conclusion, "html_url": "https://example.test/run/42",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/42/jobs":
			status, conclusion := "in_progress", ""
			if state.getRunCalls >= state.completeAt {
				status, conclusion = "completed", state.conclusion
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{{
				"id": 7, "name": "deploy", "status": status, "conclusion": conclusion,
				"steps": []map[string]any{{"number": 1, "name": "Deploy", "status": status, "conclusion": conclusion}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/jobs/7/logs":
			state.logCalls++
			if state.logCalls == 1 {
				_, _ = w.Write([]byte("line1\n"))
			} else {
				_, _ = w.Write([]byte("line1\nline2\n"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	return state, server
}

func TestShipSuccessStreamsLogs(t *testing.T) {
	state, server := newShipServer(t, "success", 3)
	defer server.Close()

	reporter := &recordingReporter{}
	run, err := Ship(context.Background(), ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml", HeadSHA: "abc123",
		Inputs:       map[string]string{"image_tag": "sha"},
		PollInterval: time.Millisecond,
		Timeout:      2 * time.Second,
		Client:       &GitHubClient{BaseURL: server.URL, Token: "token", HTTPClient: server.Client()},
		Reporter:     reporter,
	})
	if err != nil {
		t.Fatalf("Ship: %v", err)
	}
	if run.ID != 42 || run.Conclusion != "success" {
		t.Errorf("run = %+v", run)
	}
	if !reporter.started || reporter.updates == 0 {
		t.Errorf("reporter = %+v", reporter)
	}
	if got := reporter.logs(); got != "line1\nline2\n" {
		t.Errorf("logs = %q", got)
	}

	state.mu.Lock()
	dispatched := state.dispatched
	state.mu.Unlock()
	if dispatched["ref"] != "main" {
		t.Errorf("dispatch = %v", dispatched)
	}
}

func TestShipFailureConclusion(t *testing.T) {
	_, server := newShipServer(t, "failure", 1)
	defer server.Close()

	_, err := Ship(context.Background(), ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml",
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
		Client:       &GitHubClient{BaseURL: server.URL, HTTPClient: server.Client()},
	})
	if err == nil || !strings.Contains(err.Error(), "failure") {
		t.Fatalf("error = %v", err)
	}
}

func TestShipTimeout(t *testing.T) {
	_, server := newShipServer(t, "success", 9999)
	defer server.Close()

	_, err := Ship(context.Background(), ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml",
		PollInterval: 5 * time.Millisecond,
		Timeout:      40 * time.Millisecond,
		Client:       &GitHubClient{BaseURL: server.URL, HTTPClient: server.Client()},
	})
	if err == nil {
		t.Fatal("expected a timeout")
	}
}

func TestPlainReporter(t *testing.T) {
	var out strings.Builder
	reporter := NewPlainReporter(&out)
	run := WorkflowRun{ID: 1, Status: "in_progress", HTMLURL: "https://example/1"}
	reporter.RunStarted(run)
	jobs := []Job{{ID: 7, Name: "deploy", Status: "in_progress", Steps: []JobStep{{Number: 1, Name: "Checkout", Status: "completed"}}}}
	reporter.Update(run, jobs)
	reporter.Update(run, jobs) // no duplicate status lines
	reporter.Log("deploy", "hello\n")

	text := out.String()
	if strings.Count(text, "job deploy: in_progress") != 1 {
		t.Errorf("status repeated:\n%s", text)
	}
	if !strings.Contains(text, "step Checkout: completed") || !strings.Contains(text, "[deploy] hello") {
		t.Errorf("output = %q", text)
	}
}
