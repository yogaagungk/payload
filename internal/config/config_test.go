package config

import (
	"strings"
	"testing"
)

func TestDefaultAppName(t *testing.T) {
	cases := map[string]string{
		"My App_1": "my-app-1",
		"my.api":   "my-api",
		"---":      "app",
		".":        "app",
		"UPPER":    "upper",
		"a very long project name with spaces and words": "a-very-long-project-name-with-spaces-and-words",
	}
	for input, want := range cases {
		if got := DefaultAppName(input); got != want {
			t.Errorf("DefaultAppName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateAppName(t *testing.T) {
	for _, name := range []string{"app", "my-api-1", strings.Repeat("a", 63)} {
		if err := ValidateAppName(name); err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "App", "-app", "app-", "app_name", strings.Repeat("a", 64)} {
		if err := ValidateAppName(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestValidatePort(t *testing.T) {
	for _, port := range []int{1, 80, 3000, 65535} {
		if err := ValidatePort(port); err != nil {
			t.Errorf("%d rejected: %v", port, err)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if err := ValidatePort(port); err == nil {
			t.Errorf("%d accepted", port)
		}
	}
	if _, err := ParsePort("abc"); err == nil {
		t.Error("ParsePort accepted a non-number")
	}
}

func TestValidateHealthPath(t *testing.T) {
	for _, path := range []string{"/", "/health", "/api/health?full=1"} {
		if err := ValidateHealthPath(path); err != nil {
			t.Errorf("%q rejected: %v", path, err)
		}
	}
	for _, path := range []string{"", "health", "/health path", "/\"quoted\""} {
		if err := ValidateHealthPath(path); err == nil {
			t.Errorf("%q accepted", path)
		}
	}
}
