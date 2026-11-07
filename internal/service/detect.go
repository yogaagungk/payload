package service

import (
	"os"
	"path/filepath"

	"github.com/yogaagungk/payload/internal/domain"
)

// DetectStack infers a stack from common project files, or returns "".
func DetectStack(dir string) domain.Stack {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return domain.StackGo
	case exists("package.json"):
		return domain.StackNode
	case exists("requirements.txt"), exists("pyproject.toml"), exists("main.py"), exists("app.py"):
		return domain.StackPython
	case exists("index.html"):
		return domain.StackStatic
	default:
		return ""
	}
}
