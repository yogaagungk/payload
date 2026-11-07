package service

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

var repoPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Git runs read-only metadata commands and pushes through the local git CLI.
type Git struct {
	Dir string
}

func (g Git) run(ctx context.Context, args ...string) (string, error) {
	// #nosec G204 -- fixed git binary with argument slices, no shell.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentBranch returns the checked-out branch name.
func (g Git) CurrentBranch(ctx context.Context) (string, error) {
	return g.run(ctx, "symbolic-ref", "--short", "HEAD")
}

// HeadSHA returns the current commit hash.
func (g Git) HeadSHA(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "HEAD")
}

// RemoteURL returns the origin URL.
func (g Git) RemoteURL(ctx context.Context) (string, error) {
	return g.run(ctx, "config", "--get", "remote.origin.url")
}

// Status returns porcelain status output (empty when clean).
func (g Git) Status(ctx context.Context) (string, error) {
	return g.run(ctx, "status", "--porcelain")
}

// Push pushes HEAD to branch on remote.
func (g Git) Push(ctx context.Context, remote, branch string) error {
	_, err := g.run(ctx, "push", remote, "HEAD:"+branch)
	return err
}

// SplitRepo validates owner/name.
func SplitRepo(value string) (string, string, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return "", "", fmt.Errorf("repository must be owner/name, got %q", value)
	}
	return parts[0], parts[1], nil
}

func validRepoPart(part string) bool {
	return part != "" && part != "." && part != ".." && repoPartRE.MatchString(part)
}

// ParseRepoURL extracts owner/name from https, ssh and scp-style remotes.
func ParseRepoURL(raw string) (string, string, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimSuffix(value, "/")
	value = strings.TrimSuffix(value, ".git")

	if strings.HasPrefix(value, "git@") {
		if _, path, ok := strings.Cut(strings.TrimPrefix(value, "git@"), ":"); ok {
			return SplitRepo(path)
		}
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", "", fmt.Errorf("parse remote URL: %w", err)
		}
		return SplitRepo(strings.TrimPrefix(parsed.Path, "/"))
	}
	return SplitRepo(value)
}
