// Package config validates payload project settings and defaults.
package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// DefaultWorkflowPath is where payload creates the deployment workflow.
const DefaultWorkflowPath = ".github/workflows/deploy.yml"

// DefaultHealthPath is used when the user does not supply one.
const DefaultHealthPath = "/"

var appNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var healthPathRE = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$`)

// ValidateAppName checks the Docker/systemd-safe application name.
func ValidateAppName(name string) error {
	if !appNameRE.MatchString(name) {
		return fmt.Errorf("app name %q must match ^[a-z0-9][a-z0-9-]{0,62}$", name)
	}
	return nil
}

// ValidatePort checks a TCP port.
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d must be between 1 and 65535", port)
	}
	return nil
}

// ValidateHealthPath checks the HTTP path used by the deploy health check.
func ValidateHealthPath(path string) error {
	if !strings.HasPrefix(path, "/") || !healthPathRE.MatchString(path) {
		return fmt.Errorf("health path %q must start with / and contain no spaces or quotes", path)
	}
	return nil
}

// DefaultAppName derives a valid app name from a directory name.
func DefaultAppName(dir string) string {
	base := strings.ToLower(filepath.Base(filepath.Clean(dir)))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "app"
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "app"
	}
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if !appNameRE.MatchString(name) {
		return "app"
	}
	return name
}

// ParsePort parses a port string.
func ParsePort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", value)
	}
	if err := ValidatePort(port); err != nil {
		return 0, err
	}
	return port, nil
}
