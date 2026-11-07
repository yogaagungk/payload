package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
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

func TestGitMetadataAndPush(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	gitRun(t, base, "init", "--bare", bare)
	gitRun(t, work, "init", "-b", "main")
	gitRun(t, work, "config", "user.name", "Test")
	gitRun(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "file.txt")
	gitRun(t, work, "commit", "-m", "initial")
	gitRun(t, work, "remote", "add", "origin", bare)

	git := Git{Dir: work}
	ctx := context.Background()

	branch, err := git.CurrentBranch(ctx)
	if err != nil || branch != "main" {
		t.Fatalf("branch = %q, err = %v", branch, err)
	}
	sha, err := git.HeadSHA(ctx)
	if err != nil || len(sha) != 40 {
		t.Fatalf("sha = %q, err = %v", sha, err)
	}
	remoteURL, err := git.RemoteURL(ctx)
	if err != nil || remoteURL != bare {
		t.Fatalf("remote = %q, err = %v", remoteURL, err)
	}
	if err := git.Push(ctx, "origin", "main"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	pushed := gitRun(t, base, "--git-dir", bare, "rev-parse", "main")
	if len(pushed) < 40 || pushed[:40] != sha {
		t.Errorf("pushed = %q, want %q", pushed, sha)
	}
}

func TestParseRepoURL(t *testing.T) {
	valid := map[string]string{
		"https://github.com/owner/repo.git": "owner/repo",
		"git@github.com:owner/repo.git":     "owner/repo",
		"ssh://git@github.com/owner/repo":   "owner/repo",
		"owner/repo":                        "owner/repo",
	}
	for raw, want := range valid {
		owner, name, err := ParseRepoURL(raw)
		if err != nil || owner+"/"+name != want {
			t.Errorf("%q = %s/%s, err=%v", raw, owner, name, err)
		}
	}
	for _, raw := range []string{"owner", "owner/repo/extra", "../repo"} {
		if _, _, err := ParseRepoURL(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}
