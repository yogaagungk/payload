# payload

ORBIT Suite deployment CLI: `payload init` generates a Dockerfile and a
GitHub Actions deployment workflow (build, push to GHCR, deploy over SSH with
a health check and rollback), and merges into existing workflows without
losing manual edits.

Status: P1 (`init`). `ship` (push, dispatch, log streaming, approval) is P2/P3.

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
```

Flags: `--stack go|node|python|static`, `--port`, `--app`, `--health-path`,
`--dir`, `--workflow`, `--yes`.

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

The generated workflow passes `actionlint`.

## Generated deployment flow (ADR-006)

1. Checkout, then `docker/build-push-action` pushes
   `ghcr.io/<owner>/<repo>:<sha>` and `:latest` (permissions: contents read,
   packages write).
2. SSH with `StrictHostKeyChecking=yes` and `DEPLOY_KNOWN_HOSTS`; the key is
   written from `DEPLOY_SSH_KEY` to a 0600 file.
3. On the VPS: `docker login ghcr.io`, pull, run `<app>-next` with
   `--env-file /var/lib/lander/env/<app>.env` bound to `127.0.0.1:<port>`,
   wait up to 30 s for the health path, then swap containers. A failed health
   check removes the new container and keeps the previous one running.
4. `workflow_dispatch` accepts `image_tag` for rollback to an earlier image.

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
