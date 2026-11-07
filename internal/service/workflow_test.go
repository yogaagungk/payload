package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		"id: payload-validate",
		"id: payload-checkout",
		"ref: ${{ github.event.inputs.sha || github.sha }}",
		`release_id:`,
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
	// #nosec G304 -- path is built by the test itself.
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

func TestDeployScriptSafetyAndPorts(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := deployScript(t, data)
	if script == "" {
		t.Fatal("deploy step has no run script")
	}

	var candidateLine, finalLine string
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, `--name "$NEXT"`) && strings.Contains(line, "docker run") {
			candidateLine = line
		}
		if strings.Contains(line, `--name "$APP_NAME"`) && strings.Contains(line, "docker run") {
			finalLine = line
		}
	}
	if candidateLine == "" || finalLine == "" {
		t.Fatalf("docker run lines missing:\n%s", script)
	}
	if !strings.Contains(candidateLine, `127.0.0.1::${CONTAINER_PORT}`) {
		t.Errorf("candidate must use an ephemeral host port: %s", candidateLine)
	}
	if strings.Contains(candidateLine, "${APP_PORT}") {
		t.Errorf("candidate must not bind APP_PORT: %s", candidateLine)
	}
	if !strings.Contains(finalLine, `127.0.0.1:${APP_PORT}:${CONTAINER_PORT}`) {
		t.Errorf("final container must bind APP_PORT: %s", finalLine)
	}

	for _, want := range []string{
		`docker port "$NEXT"`,
		`docker start "$OLD"`,
		`image_tag_re=`,
		`[[ "${IMAGE_TAG}" =~ $image_tag_re ]]`,
		`BatchMode=yes`,
		`IdentitiesOnly=yes`,
		`${DEPLOY_PORT:-22}`,
		`bash -s`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
	if strings.Contains(script, "IMAGE='${IMAGE}") {
		t.Error("values are still interpolated into the remote command")
	}

	if binary, err := exec.LookPath("shellcheck"); err == nil {
		path := filepath.Join(t.TempDir(), "deploy-step.sh")
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		// #nosec G204 -- shellcheck path comes from LookPath; argument is a test file.
		out, err := exec.Command(binary, "-s", "bash", path).CombinedOutput()
		if err != nil {
			t.Fatalf("shellcheck failed: %v\n%s", err, out)
		}
	}
}

func deployScript(t *testing.T, data []byte) string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := documentRoot(&doc)
	steps := mapGet(mapGet(mapGet(root, "jobs"), "deploy"), "steps")
	if steps == nil {
		return ""
	}
	for _, step := range steps.Content {
		if stepID(step) == "payload-deploy" {
			run := mapGet(step, "run")
			if run != nil {
				return run.Value
			}
		}
	}
	return ""
}

func TestPlanWorkflowUnmanaged(t *testing.T) {
	existing := []byte(`name: my deploy
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo test
`)
	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanWorkflow(path, testOptions())
	if err != nil {
		t.Fatalf("PlanWorkflow: %v", err)
	}
	if plan.Managed {
		t.Error("unmanaged workflow reported as managed")
	}
	if len(plan.Warnings) == 0 {
		t.Error("expected a trigger warning for a non-mapping 'on'")
	}
	if !plan.Changed() {
		t.Fatal("plan did not propose changes")
	}
	if !strings.Contains(plan.Diff(), "proposed") {
		t.Error("diff missing header")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(plan.Updated, &doc); err != nil {
		t.Fatal(err)
	}
	root := documentRoot(&doc)
	on := mapGet(root, "on")
	if on == nil || on.Kind != yaml.SequenceNode {
		t.Errorf("existing trigger list was rewritten: %+v", on)
	}
	if on != nil && mapGet(on, "workflow_dispatch") != nil {
		t.Error("workflow_dispatch was added against a non-mapping 'on'")
	}
	if mapGet(mapGet(root, "jobs"), "test") == nil {
		t.Error("custom job was lost")
	}

	// Plan must not have written anything.
	// #nosec G304 -- path is built by the test itself.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, existing) {
		t.Error("PlanWorkflow modified the file")
	}

	if err := plan.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// #nosec G304 -- path is built by the test itself.
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, plan.Updated) {
		t.Error("written workflow differs from the plan")
	}
	// #nosec G304 -- path is built by the test itself.
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if !bytes.Equal(backup, existing) {
		t.Error("backup does not hold the original")
	}
}

func TestPlanWorkflowManagedDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.yml")
	existing := []byte("jobs:\n  deploy:\n    steps:\n      - id: payload-checkout\n")
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanWorkflow(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Managed {
		t.Error("managed workflow not detected")
	}
}

func TestPlanWorkflowUnmanagedMappingKeepsTriggers(t *testing.T) {
	existing := []byte(`name: custom
on:
  push:
    branches: [develop]
jobs:
  custom:
    runs-on: ubuntu-latest
    steps:
      - run: echo custom
`)
	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanWorkflow(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(plan.Updated, &doc); err != nil {
		t.Fatal(err)
	}
	root := documentRoot(&doc)
	on := mapGet(root, "on")
	push := mapGet(on, "push")
	branches := mapGet(push, "branches")
	if branches == nil || len(branches.Content) != 1 || branches.Content[0].Value != "develop" {
		t.Errorf("push trigger was rewritten: %+v", push)
	}
	dispatch := mapGet(on, "workflow_dispatch")
	if dispatch == nil || mapGet(mapGet(dispatch, "inputs"), "image_tag") == nil {
		t.Error("workflow_dispatch.inputs.image_tag was not added")
	}
	if mapGet(mapGet(root, "jobs"), "custom") == nil {
		t.Error("custom job was lost")
	}
}
