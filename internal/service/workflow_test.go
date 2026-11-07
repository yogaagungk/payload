package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yogaagungk/payload/internal/domain"
)

func testOptions() domain.ProjectOptions {
	return domain.ProjectOptions{AppName: "demo-api", Stack: domain.StackGo, Port: 3000, HealthPath: "/healthz"}
}

func TestWorkflowYAML(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatalf("WorkflowYAML: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"Managed by payload",
		"name: deploy",
		"APP_NAME: \"demo-api\"",
		"APP_PORT: \"3000\"",
		"CONTAINER_PORT: \"3000\"",
		"HEALTH_PATH: \"/healthz\"",
		"id: payload-checkout",
		"id: payload-build",
		"id: payload-deploy",
		"StrictHostKeyChecking=yes",
		"/var/lib/lander/env/${APP_NAME}.env",
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("workflow missing %q", want)
		}
	}
	if len(text) == 0 {
		t.Fatal("empty workflow")
	}

	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	runActionlint(t, path)
}

func TestUpdateWorkflowIsIdempotent(t *testing.T) {
	opts := testOptions()
	first, err := WorkflowYAML(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := UpdateWorkflow(first, opts)
	if err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second pass changed the file:\n--- first\n%s\n--- second\n%s", first, second)
	}
}

func TestUpdateWorkflowPreservesManualEdits(t *testing.T) {
	opts := testOptions()
	first, err := WorkflowYAML(opts)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate user edits: custom env key, custom step with a comment, custom job.
	var doc yaml.Node
	if err := yaml.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	root := documentRoot(&doc)
	env := mapGet(root, "env")
	mapSet(env, "CUSTOM_FLAG", scalarNode("keep"))

	var stepDoc yaml.Node
	if err := yaml.Unmarshal([]byte("- name: Custom test\n  run: echo hi\n"), &stepDoc); err != nil {
		t.Fatal(err)
	}
	customStep := stepDoc.Content[0].Content[0]
	customStep.HeadComment = "keep this comment"
	deploy := mapGet(mapGet(root, "jobs"), "deploy")
	steps := mapGet(deploy, "steps")
	steps.Content = append(steps.Content, customStep)

	var jobDoc yaml.Node
	if err := yaml.Unmarshal([]byte("migrate:\n  runs-on: ubuntu-latest\n  steps:\n    - run: echo migrate\n"), &jobDoc); err != nil {
		t.Fatal(err)
	}
	jobs := mapGet(root, "jobs")
	jobs.Content = append(jobs.Content, jobDoc.Content[0].Content...)

	edited, err := marshalDocument(&doc)
	if err != nil {
		t.Fatal(err)
	}

	changedOpts := opts
	changedOpts.Port = 4000
	updated, err := UpdateWorkflow(edited, changedOpts)
	if err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	text := string(updated)
	for _, want := range []string{
		"CUSTOM_FLAG",
		"Custom test",
		"keep this comment",
		"migrate",
		"APP_PORT: \"4000\"",
		"CONTAINER_PORT: \"4000\"",
		"HEALTH_PATH: \"/healthz\"",
	} {
		if !bytes.Contains(updated, []byte(want)) {
			t.Errorf("updated workflow lost %q:\n%s", want, text)
		}
	}

	// A second run must be a no-op even with the manual edits.
	again, err := UpdateWorkflow(updated, changedOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(updated, again) {
		t.Error("merge is not idempotent with manual edits")
	}

	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	runActionlint(t, path)
}

func TestUpdateWorkflowRejectsBrokenYAML(t *testing.T) {
	broken := []byte("jobs: [\n")
	if _, err := UpdateWorkflow(broken, testOptions()); err == nil {
		t.Fatal("expected a parse error")
	}

	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWorkflow(path, testOptions()); err == nil {
		t.Fatal("expected EnsureWorkflow to fail")
	}
	// #nosec G304 -- path is built by the test.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, broken) {
		t.Error("broken workflow was overwritten")
	}
}

func TestEnsureWorkflowCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".github", "workflows", "deploy.yml")
	changed, err := EnsureWorkflow(path, testOptions())
	if err != nil {
		t.Fatalf("EnsureWorkflow: %v", err)
	}
	if !changed {
		t.Error("new workflow reported unchanged")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workflow not created: %v", err)
	}
}

func TestFindWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflows := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0o750); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(workflows, "release.yml")
	if err := os.WriteFile(custom, []byte("jobs:\n  deploy:\n    steps:\n      - id: payload-checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := FindWorkflow(dir); got != custom {
		t.Errorf("FindWorkflow = %q, want %q", got, custom)
	}

	empty := t.TempDir()
	if got := FindWorkflow(empty); got != filepath.Join(empty, ".github", "workflows", "deploy.yml") {
		t.Errorf("default path = %q", got)
	}
}

func runActionlint(t *testing.T, path string) {
	t.Helper()
	binary, err := exec.LookPath("actionlint")
	if err != nil {
		t.Skip("actionlint is not installed")
	}
	// #nosec G204 -- actionlint binary path comes from LookPath; argument is a test file.
	out, err := exec.Command(binary, path).CombinedOutput()
	if err != nil {
		t.Fatalf("actionlint failed: %v\n%s", err, out)
	}
}
