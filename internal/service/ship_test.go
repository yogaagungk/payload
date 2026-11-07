package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type fakeRun struct {
	id         int64
	headSHA    string
	path       string
	created    time.Time
	status     string
	conclusion string
	calls      int
	logCalls   int
}

type shipServer struct {
	mu          sync.Mutex
	runs        map[int64]*fakeRun
	nextID      int64
	completeAt  int
	conclusion  string
	dispatched  []map[string]any
	rateLimited bool
}

func newShipServer(t *testing.T, conclusion string, completeAt int) (*shipServer, *httptest.Server) {
	t.Helper()
	state := &shipServer{
		runs:       map[int64]*fakeRun{},
		nextID:     41,
		completeAt: completeAt,
		conclusion: conclusion,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			state.dispatched = append(state.dispatched, body)
			state.nextID++
			state.runs[state.nextID] = &fakeRun{
				id:      state.nextID,
				headSHA: "abc123",
				path:    ".github/workflows/deploy.yml",
				created: time.Now().UTC(),
				status:  "queued",
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs":
			runs := make([]map[string]any, 0, len(state.runs))
			for _, run := range state.runs {
				runs = append(runs, map[string]any{
					"id": run.id, "head_sha": run.headSHA, "path": run.path, "status": run.status,
					"html_url": fmt.Sprintf("https://example.test/run/%d", run.id), "created_at": run.created,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/runs/"):
			rest := strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/runs/")
			parts := strings.Split(rest, "/")
			id, _ := strconv.ParseInt(parts[0], 10, 64)
			run := state.runs[id]
			if run == nil {
				http.NotFound(w, r)
				return
			}
			if len(parts) > 1 && parts[1] == "jobs" {
				if state.rateLimited {
					state.rateLimited = false
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				run.calls++
				status, conclusion := "in_progress", ""
				if run.calls >= state.completeAt {
					status, conclusion = "completed", state.conclusion
					run.status, run.conclusion = status, conclusion
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{{
					"id": id * 10, "name": "deploy", "status": status, "conclusion": conclusion,
					"steps": []map[string]any{{"number": 1, "name": "Deploy", "status": status, "conclusion": conclusion}},
				}}})
				return
			}
			run.calls++
			status, conclusion := "in_progress", ""
			if run.calls >= state.completeAt {
				status, conclusion = "completed", state.conclusion
				run.status, run.conclusion = status, conclusion
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "status": status, "conclusion": conclusion,
				"html_url": fmt.Sprintf("https://example.test/run/%d", id), "created_at": run.created,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/jobs/"):
			rest := strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/jobs/")
			parts := strings.Split(rest, "/")
			jobID, _ := strconv.ParseInt(parts[0], 10, 64)
			id := jobID / 10
			run := state.runs[id]
			if run == nil {
				http.NotFound(w, r)
				return
			}
			run.logCalls++
			if run.logCalls == 1 {
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

func (s *shipServer) addRun(id int64, headSHA, path, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[id] = &fakeRun{
		id: id, headSHA: headSHA, path: path, status: status,
		created: time.Now().Add(-10 * time.Minute).UTC(),
	}
	if id > s.nextID {
		s.nextID = id
	}
}

func (s *shipServer) dispatchedBodies() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.dispatched))
	copy(out, s.dispatched)
	return out
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

	dispatched := state.dispatchedBodies()
	if len(dispatched) != 1 || dispatched[0]["ref"] != "main" {
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

func TestShipIgnoresPreExistingRun(t *testing.T) {
	state, server := newShipServer(t, "success", 1)
	defer server.Close()
	state.addRun(41, "abc123", ".github/workflows/deploy.yml", "success")

	run, err := Ship(context.Background(), ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml", HeadSHA: "abc123",
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
		Client:       &GitHubClient{BaseURL: server.URL, HTTPClient: server.Client()},
	})
	if err != nil {
		t.Fatalf("Ship: %v", err)
	}
	if run.ID != 42 {
		t.Errorf("followed run %d, want the newly dispatched 42", run.ID)
	}
}

func TestShipSequentialRuns(t *testing.T) {
	_, server := newShipServer(t, "success", 1)
	defer server.Close()
	opts := ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml", HeadSHA: "abc123",
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
		Client:       &GitHubClient{BaseURL: server.URL, HTTPClient: server.Client()},
	}

	first, err := Ship(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Ship: %v", err)
	}
	second, err := Ship(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Ship: %v", err)
	}
	if first.ID == second.ID {
		t.Errorf("both ships followed run %d", first.ID)
	}
	if first.ID != 42 || second.ID != 43 {
		t.Errorf("runs = %d, %d, want 42 then 43", first.ID, second.ID)
	}
}

func TestShipHonorsRateLimit(t *testing.T) {
	state, server := newShipServer(t, "success", 1)
	defer server.Close()
	state.rateLimited = true

	_, err := Ship(context.Background(), ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml",
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
		Client:       &GitHubClient{BaseURL: server.URL, HTTPClient: server.Client()},
	})
	if err != nil {
		t.Fatalf("Ship did not retry after 429: %v", err)
	}
}

func TestPollDelayBackoff(t *testing.T) {
	cases := []struct {
		base, elapsed, want time.Duration
	}{
		{2 * time.Second, 30 * time.Second, 2 * time.Second},
		{2 * time.Second, 2 * time.Minute, 5 * time.Second},
		{2 * time.Second, 11 * time.Minute, 10 * time.Second},
		{30 * time.Second, 11 * time.Minute, 30 * time.Second},
	}
	for _, tt := range cases {
		if got := pollDelay(tt.base, tt.elapsed); got != tt.want {
			t.Errorf("pollDelay(%s, %s) = %s, want %s", tt.base, tt.elapsed, got, tt.want)
		}
	}
}
