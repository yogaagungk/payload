package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yogaagungk/payload/internal/service"
	"github.com/yogaagungk/payload/internal/tui"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// #nosec G204 -- fixed git binary with test-supplied argument slice.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCLIShipPushesDispatchesAndStreams(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, base, "init", "--bare", bare)
	gitCmd(t, work, "init", "-b", "main")
	gitCmd(t, work, "config", "user.name", "Test")
	gitCmd(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module example.com/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "go.mod")
	gitCmd(t, work, "commit", "-m", "initial")
	gitCmd(t, work, "remote", "add", "origin", bare)
	head := strings.TrimSpace(gitCmd(t, work, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(work, "dirty.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var dispatch map[string]any
	dispatched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
			_ = json.NewDecoder(r.Body).Decode(&dispatch)
			dispatched = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/runs":
			runs := []map[string]any{}
			if dispatched {
				runs = append(runs, map[string]any{
					"id": 42, "head_sha": head, "status": "in_progress",
					"html_url": "https://example.test/run/42", "path": ".github/workflows/deploy.yml",
					"created_at": time.Now().UTC().Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/runs/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 42, "status": "completed", "conclusion": "success", "html_url": "https://example.test/run/42",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/runs/42/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{{
				"id": 7, "name": "deploy", "status": "completed", "conclusion": "success",
				"steps": []map[string]any{{"number": 1, "name": "Deploy", "status": "completed", "conclusion": "success"}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/actions/jobs/7/logs":
			_, _ = w.Write([]byte("deployed successfully\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Chdir(work)
	t.Setenv("GITHUB_TOKEN", "test-token")

	stdout, stderr, err := runCLI(t, "ship", "--repo", "owner/repo", "--api", server.URL,
		"--plain", "--target", "main", "--poll", "1ms", "--timeout", "5s")
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	if !strings.Contains(stdout, "deployed successfully") || !strings.Contains(stdout, "run 42 finished: success") {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "uncommitted changes") {
		t.Errorf("stderr missing the dirty-tree warning: %q", stderr)
	}

	pushed := strings.TrimSpace(gitCmd(t, base, "--git-dir", bare, "rev-parse", "main"))
	if pushed != head {
		t.Errorf("pushed ref = %q, want %q", pushed, head)
	}

	mu.Lock()
	defer mu.Unlock()
	if dispatch["ref"] != "main" {
		t.Errorf("dispatch = %v", dispatch)
	}
}

func TestCLIShipRequiresToken(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, base, "init", "--bare", bare)
	gitCmd(t, work, "init", "-b", "main")
	gitCmd(t, work, "config", "user.name", "Test")
	gitCmd(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "file")
	gitCmd(t, work, "commit", "-m", "initial")
	gitCmd(t, work, "remote", "add", "origin", bare)

	t.Chdir(work)
	t.Setenv("GITHUB_TOKEN", "")
	_, _, err := runCLI(t, "ship", "--repo", "owner/repo", "--plain", "--poll", "1ms")
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error = %v, want a token error", err)
	}
	// #nosec G204 -- fixed git binary with test-supplied arguments.
	if out, pushErr := exec.Command("git", "--git-dir", bare, "rev-parse", "main").CombinedOutput(); pushErr == nil {
		t.Errorf("ref was pushed before the token check: %s", out)
	}
}

func TestRunShipTUIDetachesPromptly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dispatches"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{
				"id": 42, "head_sha": "abc", "path": ".github/workflows/deploy.yml", "status": "in_progress",
				"html_url": "https://example.test/run/42", "created_at": time.Now().UTC(),
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": "in_progress"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/42/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	opts := service.ShipOptions{
		Owner: "o", Repo: "r", Ref: "main", Workflow: "deploy.yml",
		PollInterval: 50 * time.Millisecond,
		Timeout:      10 * time.Second,
		Client:       &service.GitHubClient{BaseURL: server.URL, HTTPClient: server.Client()},
	}
	var stdout bytes.Buffer
	application := newApp(strings.NewReader(""), &stdout, &bytes.Buffer{})
	newProgram := func(model tui.Model) *tea.Program {
		return tea.NewProgram(model,
			tea.WithoutRenderer(),
			tea.WithInput(strings.NewReader("q")),
			tea.WithOutput(io.Discard),
		)
	}

	start := time.Now()
	if err := application.runShipTUIWith(context.Background(), opts, newProgram); err != nil {
		t.Fatalf("runShipTUIWith: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("detach took %s", elapsed)
	}
	if !strings.Contains(stdout.String(), "detached") {
		t.Errorf("stdout = %q, want a detach message", stdout.String())
	}
}
