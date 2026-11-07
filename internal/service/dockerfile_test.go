package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yogaagungk/payload/internal/domain"
)

func TestDockerfilePerStack(t *testing.T) {
	cases := []struct {
		stack       domain.Stack
		port        int
		wantExpose  string
		wantContent string
	}{
		{domain.StackGo, 3000, "EXPOSE 3000", "golang:1.27-alpine"},
		{domain.StackNode, 3000, "EXPOSE 3000", "node:22-alpine"},
		{domain.StackPython, 3000, "EXPOSE 3000", "python:3.12-slim"},
		{domain.StackStatic, 8080, "EXPOSE 80", "nginx:1.27-alpine"},
	}
	for _, tt := range cases {
		t.Run(string(tt.stack), func(t *testing.T) {
			opts := domain.ProjectOptions{AppName: "demo", Stack: tt.stack, Port: tt.port, HealthPath: "/"}
			content, err := Dockerfile(opts)
			if err != nil {
				t.Fatalf("Dockerfile: %v", err)
			}
			if !strings.HasPrefix(content, DockerfileMarker) {
				t.Errorf("missing marker:\n%s", content)
			}
			for _, want := range []string{"FROM ", tt.wantExpose, tt.wantContent} {
				if !strings.Contains(content, want) {
					t.Errorf("Dockerfile missing %q:\n%s", want, content)
				}
			}
		})
	}
}

func TestEnsureDockerfileKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	existing := []byte("FROM scratch\n# my own file\n")
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}

	gotPath, created, err := EnsureDockerfile(dir, domain.ProjectOptions{Stack: domain.StackGo, Port: 3000})
	if err != nil {
		t.Fatalf("EnsureDockerfile: %v", err)
	}
	if created {
		t.Error("existing Dockerfile reported as created")
	}
	if gotPath != path {
		t.Errorf("path = %q", gotPath)
	}
	// #nosec G304 -- path is built by the test.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(existing) {
		t.Error("existing Dockerfile was overwritten")
	}
}

func TestEnsureDockerfileCreates(t *testing.T) {
	dir := t.TempDir()
	_, created, err := EnsureDockerfile(dir, domain.ProjectOptions{Stack: domain.StackNode, Port: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("Dockerfile not created")
	}
	// #nosec G304 -- path is built by the test.
	content, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "EXPOSE 3000") {
		t.Errorf("content = %s", content)
	}
}
