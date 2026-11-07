package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	notFoundSSH = `#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >> "${SHIM_LOG:?}/ssh.argv"
remote="${!#}"
exec bash -c "$remote"
`
	notFoundDocker = `#!/usr/bin/env bash
set -u
log="${SHIM_LOG:?}"
printf '%s\n' "$*" >> "$log/docker.argv"
cmd="${1:-}"
case "$cmd" in
  login)
    cat > "$log/docker.login.stdin"
    exit 0
    ;;
  run)
    name=""
    publish=""
    args=("$@")
    for ((i=1; i<${#args[@]}; i++)); do
      if [ "${args[$i]}" = "--name" ]; then name="${args[$((i+1))]}"; fi
      if [ "${args[$i]}" = "-p" ]; then publish="${args[$((i+1))]}"; fi
    done
    if [[ "$name" == *-next ]]; then
      printf '%s\n' "$publish" > "$log/candidate.publish"
      printf '%s\n' "127.0.0.1:32768" > "$log/port.out"
    else
      printf '%s\n' "$publish" > "$log/final.publish"
      if [ "${FAKE_FINAL_RUN_FAIL:-0}" = "1" ]; then exit 1; fi
    fi
    exit 0
    ;;
  port)
    cat "$log/port.out" 2>/dev/null || printf '%s\n' "127.0.0.1:32768"
    exit 0
    ;;
  inspect)
    target="${2:-}"
    if [[ "$target" == *-old ]]; then
      [ "${FAKE_OLD_EXISTS:-0}" = "1" ] && exit 0
      exit 1
    fi
    [ "${FAKE_OLD_EXISTS:-0}" = "1" ] && exit 0
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`
	notFoundCurl = `#!/usr/bin/env bash
set -u
log="${SHIM_LOG:?}"
printf '%s\n' "$*" >> "$log/curl.argv"
url="${!#}"
case "${FAKE_HEALTH:-ok}" in
  candidate-fail) exit 1 ;;
  final-fail)
    if [[ "$url" == *":32768"* ]]; then exit 0; else exit 1; fi
    ;;
  *) exit 0 ;;
esac
`
)

type deployRun struct {
	logDir string
	err    error
	output string
}

func runDeployStep(t *testing.T, script string, extraEnv map[string]string) deployRun {
	t.Helper()
	binDir := t.TempDir()
	logDir := t.TempDir()
	for name, content := range map[string]string{
		"ssh":    notFoundSSH,
		"docker": notFoundDocker,
		"curl":   notFoundCurl,
		"sleep":  "#!/usr/bin/env bash\nexit 0\n",
	} {
		// #nosec G306 -- test shims must be executable.
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	scriptPath := filepath.Join(t.TempDir(), "deploy-step.sh")
	// #nosec G306 -- the test executes this script.
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"SHIM_LOG=" + logDir,
		"APP_NAME=demo-api",
		"APP_PORT=3000",
		"CONTAINER_PORT=3000",
		"HEALTH_PATH=/healthz",
		"IMAGE=ghcr.io/owner/repo",
		"IMAGE_TAG=abc123",
		"GHCR_TOKEN=super-secret-token",
		"GITHUB_ACTOR=actor",
		"DEPLOY_HOST=example.com",
		"DEPLOY_USER=deploy",
		"DEPLOY_PORT=22",
		"DEPLOY_SSH_KEY=key-material",
		"DEPLOY_KNOWN_HOSTS=host-key",
	}
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}

	// #nosec G204 -- the test executes its own generated script.
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	return deployRun{logDir: logDir, err: err, output: string(out)}
}

func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	// #nosec G304 -- path is built by the test.
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestDeployTransportEndToEnd(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := deployScript(t, data)
	if script == "" {
		t.Fatal("no deploy script")
	}

	run := runDeployStep(t, script, map[string]string{"FAKE_OLD_EXISTS": "1"})
	if run.err != nil {
		t.Fatalf("deploy script failed: %v\n%s", run.err, run.output)
	}

	dockerLog := readLog(t, run.logDir, "docker.argv")
	if !strings.Contains(dockerLog, "login ghcr.io -u actor --password-stdin") {
		t.Errorf("missing docker login:\n%s", dockerLog)
	}
	if !strings.Contains(dockerLog, "pull ghcr.io/owner/repo:abc123") {
		t.Errorf("missing docker pull:\n%s", dockerLog)
	}
	if !strings.Contains(dockerLog, "--name demo-api-next") || !strings.Contains(dockerLog, "127.0.0.1::3000") {
		t.Errorf("candidate run is wrong:\n%s", dockerLog)
	}
	if !strings.Contains(dockerLog, "--name demo-api ") || !strings.Contains(dockerLog, "127.0.0.1:3000:3000") {
		t.Errorf("final run is wrong:\n%s", dockerLog)
	}
	if !strings.Contains(dockerLog, "stop demo-api") || !strings.Contains(dockerLog, "rename demo-api demo-api-old") {
		t.Errorf("swap commands missing:\n%s", dockerLog)
	}

	candidatePublish := strings.TrimSpace(readLog(t, run.logDir, "candidate.publish"))
	finalPublish := strings.TrimSpace(readLog(t, run.logDir, "final.publish"))
	if candidatePublish == finalPublish || candidatePublish != "127.0.0.1::3000" || finalPublish != "127.0.0.1:3000:3000" {
		t.Errorf("port mappings share a host port: candidate=%q final=%q", candidatePublish, finalPublish)
	}

	login := strings.TrimSpace(readLog(t, run.logDir, "docker.login.stdin"))
	if login != "super-secret-token" {
		t.Errorf("docker login stdin = %q", login)
	}
	sshArgv := readLog(t, run.logDir, "ssh.argv")
	if strings.Contains(sshArgv, "super-secret-token") || strings.Contains(dockerLog, "super-secret-token") {
		t.Error("GHCR token leaked into argv")
	}

	// The remote one-liner itself must pass shellcheck too.
	if binary, err := exec.LookPath("shellcheck"); err == nil {
		start := strings.Index(script, "'IFS= read -r APP_NAME")
		end := strings.LastIndex(script, "exec bash -s'")
		if start < 0 || end < 0 {
			t.Fatal("remote command not found in the deploy script")
		}
		remote := script[start+1 : end+len("exec bash -s")]
		path := filepath.Join(t.TempDir(), "remote.sh")
		// #nosec G306 -- the test only reads this file.
		if err := os.WriteFile(path, []byte(remote), 0o600); err != nil {
			t.Fatal(err)
		}
		// #nosec G204 -- shellcheck path comes from LookPath.
		if out, err := exec.Command(binary, "-s", "bash", path).CombinedOutput(); err != nil {
			t.Fatalf("shellcheck remote command: %v\n%s", err, out)
		}
	}
}

func TestDeployTransportRejectsInjectedTag(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	run := runDeployStep(t, deployScript(t, data), map[string]string{
		"IMAGE_TAG": "x'; touch pwned; '",
	})
	if run.err == nil {
		t.Fatal("injected image tag was accepted")
	}
	if log := readLog(t, run.logDir, "ssh.argv"); log != "" {
		t.Errorf("ssh was invoked for an invalid tag:\n%s", log)
	}
}

func TestDeployRollbackOnCandidateFailure(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	run := runDeployStep(t, deployScript(t, data), map[string]string{
		"FAKE_OLD_EXISTS": "1",
		"FAKE_HEALTH":     "candidate-fail",
	})
	if run.err == nil {
		t.Fatal("candidate health failure did not fail the job")
	}
	dockerLog := readLog(t, run.logDir, "docker.argv")
	if !strings.Contains(dockerLog, "rm -f demo-api-next") {
		t.Errorf("candidate was not removed:\n%s", dockerLog)
	}
	if strings.Contains(dockerLog, "rename demo-api demo-api-old") {
		t.Error("old container was swapped before the candidate was healthy")
	}
}

func TestDeployRollbackOnFinalFailure(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"final-fail", "final-run-fail"} {
		t.Run(mode, func(t *testing.T) {
			extra := map[string]string{"FAKE_OLD_EXISTS": "1"}
			if mode == "final-fail" {
				extra["FAKE_HEALTH"] = "final-fail"
			} else {
				extra["FAKE_FINAL_RUN_FAIL"] = "1"
			}
			run := runDeployStep(t, deployScript(t, data), extra)
			if run.err == nil {
				t.Fatal("final failure did not fail the job")
			}
			dockerLog := readLog(t, run.logDir, "docker.argv")
			if !strings.Contains(dockerLog, "start demo-api-old") {
				t.Errorf("previous container was not restarted:\n%s", dockerLog)
			}
		})
	}
}

func TestDeployScriptOptionalEnvFile(t *testing.T) {
	data, err := WorkflowYAML(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := deployScript(t, data)

	envDir := t.TempDir()
	empty := runDeployStep(t, script, map[string]string{"LANDER_ENV_DIR": envDir, "FAKE_OLD_EXISTS": "1"})
	if empty.err != nil {
		t.Fatalf("deploy without env file: %v\n%s", empty.err, empty.output)
	}
	if strings.Contains(readLog(t, empty.logDir, "docker.argv"), "--env-file") {
		t.Error("missing env file still produced --env-file")
	}

	if err := os.WriteFile(filepath.Join(envDir, "demo-api.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	present := runDeployStep(t, script, map[string]string{"LANDER_ENV_DIR": envDir, "FAKE_OLD_EXISTS": "1"})
	if present.err != nil {
		t.Fatalf("deploy with env file: %v\n%s", present.err, present.output)
	}
	if got := readLog(t, present.logDir, "docker.argv"); !strings.Contains(got, "--env-file "+filepath.Join(envDir, "demo-api.env")) {
		t.Errorf("existing env file not passed: %s", got)
	}
}
