package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	application := newApp(strings.NewReader(""), &stdout, &stderr)
	cmd := newRootCmd(application)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestCLIInitCreatesDockerfileAndWorkflow(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("go.mod", []byte("module example.com/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCLI(t, "init", "--yes", "--app", "demo-api", "--port", "3000")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(stdout, "demo-api (go, port 3000)") {
		t.Errorf("stdout = %q", stdout)
	}
	for _, path := range []string{"Dockerfile", ".github/workflows/deploy.yml"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s not created: %v", path, err)
		}
	}

	workflow, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(workflow, []byte("id: payload-deploy")) {
		t.Error("workflow missing managed deploy step")
	}
	runActionlintCLI(t, ".github/workflows/deploy.yml")

	// A second run is a no-op.
	before, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "init", "--yes", "--app", "demo-api", "--port", "3000"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("second init changed the workflow")
	}
}

func TestCLIInitPreservesManualEdits(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("package.json", []byte("{\"name\":\"app\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "init", "--yes", "--app", "web", "--port", "3000"); err != nil {
		t.Fatal(err)
	}

	path := ".github/workflows/deploy.yml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data),
		"      - id: payload-checkout",
		"      - name: Lint\n        run: echo lint\n\n      - id: payload-checkout", 1)
	// #nosec G703 -- path is the test workflow file.
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runCLI(t, "init", "--yes", "--app", "web", "--port", "4000"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte("name: Lint")) {
		t.Error("manual step was removed")
	}
	if !bytes.Contains(updated, []byte(`APP_PORT: "4000"`)) {
		t.Error("managed port was not updated")
	}
	runActionlintCLI(t, path)
}

func TestCLIInitStackRequirements(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, err := runCLI(t, "init", "--yes"); err == nil {
		t.Error("empty project without --stack was accepted")
	}
	if _, _, err := runCLI(t, "init", "--yes", "--stack", "rust"); err == nil {
		t.Error("unsupported stack was accepted")
	}
	if _, _, err := runCLI(t, "init", "--yes", "--stack", "static", "--app", "Bad App"); err == nil {
		t.Error("invalid app name was accepted")
	}
}

func TestCLIInitCustomWorkflowPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("src", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("index.html", []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "init", "--yes", "--workflow", ".github/workflows/release.yml"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := os.Stat(".github/workflows/release.yml"); err != nil {
		t.Fatalf("custom workflow missing: %v", err)
	}
	// Static sites use nginx port 80 inside the container.
	data, _ := os.ReadFile("Dockerfile")
	if !bytes.Contains(data, []byte("EXPOSE 80")) {
		t.Errorf("static Dockerfile = %s", data)
	}
}

func TestCLIInitDetectsStack(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("requirements.txt", []byte("flask\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, "init", "--yes", "--app", "py-app")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(stdout, "py-app (python, port 3000)") {
		t.Errorf("stdout = %q", stdout)
	}
}

func runActionlintCLI(t *testing.T, path string) {
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

func TestCLIInitWizard(t *testing.T) {
	t.Chdir(t.TempDir())
	var stdout bytes.Buffer
	application := newApp(strings.NewReader("1\n3200\nwizard-app\n/health\n"), &stdout, &bytes.Buffer{})
	application.interactive = true

	cmd := newRootCmd(application)
	cmd.SetArgs([]string{"init"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wizard init: %v", err)
	}
	if !strings.Contains(stdout.String(), "wizard-app (go, port 3200)") {
		t.Errorf("stdout = %q", stdout.String())
	}
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`APP_NAME: "wizard-app"`, `APP_PORT: "3200"`, `HEALTH_PATH: "/health"`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("workflow missing %q", want)
		}
	}
}

func TestResolveStackByName(t *testing.T) {
	stack, err := resolveStack("Python", t.TempDir(), false, nil, &bytes.Buffer{})
	if err != nil || stack != "python" {
		t.Fatalf("stack = %q, err = %v", stack, err)
	}
}
