# payload

ORBIT Suite deployment CLI: `payload init` generates a Dockerfile and a
GitHub Actions deployment workflow (build, push to GHCR, deploy over SSH with
a health check and rollback), and merges into existing workflows without
losing manual edits.

Status: P1 (`init`) and P2 (`ship`: push, dispatch, status/log streaming).
The `--require-approval` flow is P3.

## Build

```sh
make build       # bin/payload
make test        # go test -race ./...
make vet
make lint
make actionlint  # requires actionlint on PATH
make install
```

## Usage

```sh
# Interactive wizard: stack, port, app name, health path
payload init

# Non-interactive (stack auto-detected from go.mod/package.json/
# requirements.txt/index.html when omitted)
payload init --stack go --port 3000 --app my-api --yes

# Write to a different workflow file
payload init --stack static --app site --workflow .github/workflows/release.yml

# Push the current branch, dispatch deploy.yml and stream the run
GITHUB_TOKEN=... payload ship

# Explicit branch/workflow/input, plain output
payload ship --target main --workflow deploy.yml \
  --input image_tag=abc123 --plain

# Already pushed: skip git push
payload ship --no-push
```

Flags: `--stack go|node|python|static`, `--port`, `--app`, `--health-path`,
`--dir`, `--workflow`, `--yes`, `--merge`, `--diff`.

`init` writes `Dockerfile` only when none exists, and creates or updates a
workflow (default `.github/workflows/deploy.yml`, or the first existing
workflow that contains a `payload-*` step). Re-running is idempotent:

- Steps with `id: payload-checkout`, `payload-build`, `payload-deploy` are
  replaced with the current template.
- Managed `env` keys (`APP_NAME`, `APP_PORT`, `CONTAINER_PORT`,
  `HEALTH_PATH`, `IMAGE`, `IMAGE_TAG`) are updated.
- Everything else is preserved: custom jobs, custom steps, extra triggers,
  extra env keys and comments (YAML AST merge via `yaml.v3`).
- A broken existing workflow is left untouched and reported as an error.
- An existing `Dockerfile` is never overwritten.

Existing **unmanaged** workflows (no `payload-*` step) are never rewritten by
default: `init` prints a unified diff and exits non-zero. Use `--diff` for a
read-only preview or `--merge` to apply; the first merge writes
`deploy.yml.bak`, and existing triggers are kept as written (only a missing
`workflow_dispatch.inputs.image_tag` is added to a mapping `on`). If `on` is a
list or scalar it is left alone and a warning is printed.

The generated workflow passes `actionlint`.

## ship

`payload ship` pushes `HEAD` to the target branch (`--target`, default the
current branch) with the local git CLI, dispatches
`POST /repos/{owner}/{repo}/actions/workflows/{workflow}/dispatches`, then
polls the newest matching `workflow_dispatch` run (matched by `head_sha`).

- Default output is a bubbletea TUI with run/job/step status and log tail;
  `q`/Esc detaches and leaves the run going on GitHub. `--plain` (or a
  non-terminal stdout) streams plain text; SIGINT cancels the wait.
- GitHub returns job logs only when they become available; statuses are live
  and each job's log is printed incrementally as the API serves it. A failed
  run exits non-zero after printing the conclusion and run URL.
- `--poll` (default 2s, backing off to 5s/10s) and `--timeout` (default 30m)
  control waiting; 403/429 responses honour `Retry-After`/rate-limit reset.
- The token is checked before the push, so a missing token never leaves a
  half-done ship. Dirty trees and a `--target` different from the current
  branch produce a warning; if no matching run appears after 60 s a hint is
  printed. Log output is stripped of terminal control sequences except
  newlines, tabs and SGR colours.
- The token comes from `GITHUB_TOKEN` or a hidden prompt, never a flag. A
  fine-grained PAT needs Actions: write and Contents: read; a classic PAT
  needs `repo`.
- `--repo` defaults to the origin remote; `--api` must be https (http only on
  loopback for tests).
- `--require-approval` posts the release to houston (HOUSTON_URL,
  HOUSTON_WEBHOOK_TOKEN), polls until dispatched/rejected/expired/
  dispatch_failed, then streams the run (by `run_id`, or by finding the new
  run itself when houston left it null). Exit codes: rejected 3, expired 4,
  dispatch_failed 5, timeout 6.

## Generated deployment flow (ADR-006)

1. Checkout, then `docker/build-push-action` pushes
   `ghcr.io/<owner>/<repo>:<sha>` and `:latest` (permissions: contents read,
   packages write).
2. SSH with `StrictHostKeyChecking=yes` and `DEPLOY_KNOWN_HOSTS`; the key is
   written from `DEPLOY_SSH_KEY` to a 0600 file.
3. Values are validated in the runner script (`image_tag`, app name, ports,
   health path) and sent to the VPS on stdin, not in the SSH command line.
4. On the VPS: `docker login ghcr.io`, pull, start `<app>-next` on an
   ephemeral loopback port, read it with `docker port`, and health-check it for
   up to 30 s. On success the old container is stopped/renamed and the final
   `<app>` starts on `127.0.0.1:<port>`; if that start or its health check
   fails, the previous `-old` container is restarted and the job fails.
5. `workflow_dispatch` accepts `image_tag` for rollback to an earlier image.

Verify on the first real deploy (E1) that the workflow's `GITHUB_TOKEN` can
pull the private GHCR package on the VPS; this is not exercised by local
tests.

Required repository secrets: `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_PORT`,
`DEPLOY_SSH_KEY`, `DEPLOY_KNOWN_HOSTS` (for example via `blackbox sync`).

## Stack assumptions

- Go: builds `./...` and serves on the configured port.
- Node: uses `npm start`; `package.json` must define a start script.
- Python: runs `python main.py`; adjust the CMD for other entrypoints.
- Static: nginx on container port 80.

## Layout

```text
cmd/payload        cobra commands (init, version)
internal/config    validation, defaults, app-name derivation
internal/domain    stack and option models
internal/service   Dockerfile templates, stack detection, YAML AST merge
```
