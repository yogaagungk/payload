package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yogaagungk/payload/internal/domain"
)

func TestDetectStack(t *testing.T) {
	cases := map[string]domain.Stack{
		"go.mod":           domain.StackGo,
		"package.json":     domain.StackNode,
		"requirements.txt": domain.StackPython,
		"index.html":       domain.StackStatic,
	}
	for file, want := range cases {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := DetectStack(dir); got != want {
				t.Errorf("DetectStack = %q, want %q", got, want)
			}
		})
	}

	if got := DetectStack(t.TempDir()); got != "" {
		t.Errorf("empty dir = %q", got)
	}
}
