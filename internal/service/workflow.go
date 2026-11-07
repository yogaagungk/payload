package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"

	"github.com/yogaagungk/payload/internal/domain"
)

// WorkflowMarker is the header written into payload-generated workflows.
const WorkflowMarker = "Managed by payload: steps with id payload-* are updated by 'payload init'."

const workflowTemplate = `# %s
name: deploy

on:
  push:
    branches: [main]
  workflow_dispatch:
    inputs:
      image_tag:
        description: "Image tag to deploy (default: commit SHA)"
        required: false
        default: ""
        type: string

permissions:
  contents: read
  packages: write

concurrency:
  group: deploy-${{ github.ref }}
  cancel-in-progress: true

env:
  APP_NAME: "%s"
  APP_PORT: "%d"
  CONTAINER_PORT: "%d"
  HEALTH_PATH: "%s"
  IMAGE: ghcr.io/${{ github.repository }}
  IMAGE_TAG: ${{ github.event.inputs.image_tag || github.sha }}

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - id: payload-checkout
        name: Checkout
        uses: actions/checkout@v4

      - id: payload-build
        name: Build and push image
        uses: docker/build-push-action@v6
        with:
          context: .
          file: ./Dockerfile
          push: true
          tags: |
            ${{ env.IMAGE }}:${{ env.IMAGE_TAG }}
            ${{ env.IMAGE }}:latest

      - id: payload-deploy
        name: Deploy over SSH
        env:
          DEPLOY_HOST: ${{ secrets.DEPLOY_HOST }}
          DEPLOY_USER: ${{ secrets.DEPLOY_USER }}
          DEPLOY_PORT: ${{ secrets.DEPLOY_PORT }}
          DEPLOY_SSH_KEY: ${{ secrets.DEPLOY_SSH_KEY }}
          DEPLOY_KNOWN_HOSTS: ${{ secrets.DEPLOY_KNOWN_HOSTS }}
          GHCR_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          set -euo pipefail
          image_tag_re='^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$'
          app_re='^[a-z0-9][a-z0-9-]{0,62}$'
          port_re='^[0-9]{1,5}$'
          health_re='^/[A-Za-z0-9._~!$&()*+,;=:@%%/?-]*$'
          [[ "${IMAGE_TAG}" =~ $image_tag_re ]] || { echo "invalid image_tag" >&2; exit 1; }
          [[ "${APP_NAME}" =~ $app_re ]] || { echo "invalid APP_NAME" >&2; exit 1; }
          [[ "${APP_PORT}" =~ $port_re ]] || { echo "invalid APP_PORT" >&2; exit 1; }
          [[ "${CONTAINER_PORT}" =~ $port_re ]] || { echo "invalid CONTAINER_PORT" >&2; exit 1; }
          [[ "${HEALTH_PATH}" =~ $health_re ]] || { echo "invalid HEALTH_PATH" >&2; exit 1; }
          install -m 700 -d "$HOME/.ssh"
          printf '%%s\n' "${DEPLOY_SSH_KEY}" > "$HOME/.ssh/id_ed25519"
          chmod 600 "$HOME/.ssh/id_ed25519"
          printf '%%s\n' "${DEPLOY_KNOWN_HOSTS}" > "$HOME/.ssh/known_hosts"
          chmod 600 "$HOME/.ssh/known_hosts"
          {
            printf '%%s\n' "${APP_NAME}" "${APP_PORT}" "${CONTAINER_PORT}" "${HEALTH_PATH}" "${IMAGE}:${IMAGE_TAG}" "${GHCR_TOKEN}" "${GITHUB_ACTOR}"
            cat <<'EOSSH'
          set -euo pipefail
          echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
          docker pull "$IMAGE"
          NEXT="${APP_NAME}-next"
          OLD="${APP_NAME}-old"
          docker rm -f "$NEXT" >/dev/null 2>&1 || true
          docker run -d --name "$NEXT" --env-file "/var/lib/lander/env/${APP_NAME}.env" --restart unless-stopped -p "127.0.0.1::${CONTAINER_PORT}" "$IMAGE"
          candidate_port=$(docker port "$NEXT" "${CONTAINER_PORT}/tcp" | awk -F: 'NR==1 {print $NF}')
          case "$candidate_port" in
            ''|*[!0-9]*) echo "could not read candidate port" >&2; docker rm -f "$NEXT" >/dev/null 2>&1 || true; exit 1 ;;
          esac
          healthy=0
          for _ in {1..30}; do
            if curl -fsS "http://127.0.0.1:${candidate_port}${HEALTH_PATH}" >/dev/null 2>&1; then
              healthy=1
              break
            fi
            sleep 1
          done
          if [ "$healthy" -ne 1 ]; then
            docker rm -f "$NEXT" >/dev/null 2>&1 || true
            echo "health check failed; previous container kept" >&2
            exit 1
          fi
          docker rm -f "$OLD" >/dev/null 2>&1 || true
          if docker inspect "$APP_NAME" >/dev/null 2>&1; then
            docker stop "$APP_NAME" >/dev/null
            docker rename "$APP_NAME" "$OLD"
          fi
          if ! docker run -d --name "$APP_NAME" --env-file "/var/lib/lander/env/${APP_NAME}.env" --restart unless-stopped -p "127.0.0.1:${APP_PORT}:${CONTAINER_PORT}" "$IMAGE"; then
            echo "final container failed to start; restarting previous" >&2
            docker rm -f "$APP_NAME" >/dev/null 2>&1 || true
            if docker inspect "$OLD" >/dev/null 2>&1; then docker start "$OLD" >/dev/null || true; fi
            docker rm -f "$NEXT" >/dev/null 2>&1 || true
            exit 1
          fi
          healthy=0
          for _ in {1..30}; do
            if curl -fsS "http://127.0.0.1:${APP_PORT}${HEALTH_PATH}" >/dev/null 2>&1; then
              healthy=1
              break
            fi
            sleep 1
          done
          if [ "$healthy" -ne 1 ]; then
            echo "final health check failed; restarting previous" >&2
            docker rm -f "$APP_NAME" >/dev/null 2>&1 || true
            if docker inspect "$OLD" >/dev/null 2>&1; then docker start "$OLD" >/dev/null || true; fi
            docker rm -f "$NEXT" >/dev/null 2>&1 || true
            exit 1
          fi
          docker rm -f "$NEXT" >/dev/null 2>&1 || true
          EOSSH
          } | ssh -i "$HOME/.ssh/id_ed25519" \
              -o BatchMode=yes \
              -o IdentitiesOnly=yes \
              -o UserKnownHostsFile="$HOME/.ssh/known_hosts" \
              -o StrictHostKeyChecking=yes \
              -p "${DEPLOY_PORT:-22}" \
              "${DEPLOY_USER}@${DEPLOY_HOST}" \
              'IFS= read -r APP_NAME && IFS= read -r APP_PORT && IFS= read -r CONTAINER_PORT && IFS= read -r HEALTH_PATH && IFS= read -r IMAGE && IFS= read -r GHCR_TOKEN && IFS= read -r GITHUB_ACTOR && export APP_NAME APP_PORT CONTAINER_PORT HEALTH_PATH IMAGE GHCR_TOKEN GITHUB_ACTOR && exec bash -s'
`

// WorkflowYAML renders a fresh deployment workflow.
func WorkflowYAML(opts domain.ProjectOptions) ([]byte, error) {
	text := fmt.Sprintf(workflowTemplate, WorkflowMarker, opts.AppName, opts.Port,
		opts.Stack.ContainerPort(opts.Port), opts.HealthPath)
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		return nil, fmt.Errorf("parse generated workflow: %w", err)
	}
	return marshalDocument(&document)
}

// UpdateWorkflow merges payload-managed settings into an existing workflow
// document. Managed steps are identified by their payload-* id; every other
// key, step, job and comment is preserved. An empty existing document
// produces a new workflow.
func UpdateWorkflow(existing []byte, opts domain.ProjectOptions) ([]byte, error) {
	updated, _, err := updateWorkflow(existing, opts)
	return updated, err
}

func updateWorkflow(existing []byte, opts domain.ProjectOptions) ([]byte, []string, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		updated, err := WorkflowYAML(opts)
		return updated, nil, err
	}

	var current yaml.Node
	if err := yaml.Unmarshal(existing, &current); err != nil {
		return nil, nil, fmt.Errorf("parse existing workflow: %w", err)
	}
	root := documentRoot(&current)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("existing workflow is not a YAML mapping")
	}

	baselineYAML, err := WorkflowYAML(opts)
	if err != nil {
		return nil, nil, err
	}
	var baseline yaml.Node
	if err := yaml.Unmarshal(baselineYAML, &baseline); err != nil {
		return nil, nil, fmt.Errorf("parse baseline workflow: %w", err)
	}
	baseRoot := documentRoot(&baseline)

	var warnings []string
	if on := mapGet(root, "on"); on != nil && on.Kind != yaml.MappingNode {
		warnings = append(warnings, "existing 'on' is not a mapping; triggers were left unchanged (add workflow_dispatch manually for payload ship)")
	}

	ensureManagedWorkflow(root, baseRoot)
	updated, err := marshalDocument(&current)
	return updated, warnings, err
}

func ensureManagedWorkflow(root, base *yaml.Node) {
	if mapGet(root, "name") == nil {
		mapSet(root, "name", scalarNode("deploy"))
	}

	if on := mapGet(root, "on"); on == nil {
		mapSet(root, "on", cloneNode(mapGet(base, "on")))
	} else if on.Kind == yaml.MappingNode {
		mergeWorkflowDispatch(on, mapGet(base, "on"))
	}

	if permissions := mapGet(root, "permissions"); permissions == nil || permissions.Kind != yaml.MappingNode {
		mapSet(root, "permissions", cloneNode(mapGet(base, "permissions")))
	} else {
		mapSet(permissions, "contents", scalarNode("read"))
		mapSet(permissions, "packages", scalarNode("write"))
	}

	if mapGet(root, "concurrency") == nil {
		mapSet(root, "concurrency", cloneNode(mapGet(base, "concurrency")))
	}

	env := ensureMapping(mapGet(root, "env"))
	if env == nil {
		env = mappingNode()
		mapSet(root, "env", env)
	}
	baseEnv := mapGet(base, "env")
	for _, key := range []string{"APP_NAME", "APP_PORT", "CONTAINER_PORT", "HEALTH_PATH", "IMAGE", "IMAGE_TAG"} {
		mapSet(env, key, cloneNode(mapGet(baseEnv, key)))
	}

	jobs := ensureMapping(mapGet(root, "jobs"))
	if jobs == nil {
		jobs = mappingNode()
		mapSet(root, "jobs", jobs)
	}
	deploy := mapGet(jobs, "deploy")
	baseDeploy := mapGet(mapGet(base, "jobs"), "deploy")
	if deploy == nil || deploy.Kind != yaml.MappingNode {
		mapSet(jobs, "deploy", cloneNode(baseDeploy))
		return
	}
	if mapGet(deploy, "runs-on") == nil {
		mapSet(deploy, "runs-on", scalarNode("ubuntu-latest"))
	}
	steps := mapGet(deploy, "steps")
	baseSteps := mapGet(baseDeploy, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		mapSet(deploy, "steps", cloneNode(baseSteps))
		return
	}
	for _, baseStep := range baseSteps.Content {
		id := stepID(baseStep)
		if id == "" {
			continue
		}
		if idx := findStep(steps, id); idx >= 0 {
			steps.Content[idx] = cloneNode(baseStep)
			continue
		}
		steps.Content = append(steps.Content, cloneNode(baseStep))
	}
}

// mergeWorkflowDispatch adds the dispatch trigger or its image_tag input
// without touching any existing trigger the user wrote.
func mergeWorkflowDispatch(on, baseOn *yaml.Node) {
	baseDispatch := mapGet(baseOn, "workflow_dispatch")
	if baseDispatch == nil {
		return
	}
	dispatch := mapGet(on, "workflow_dispatch")
	if dispatch == nil || dispatch.Kind != yaml.MappingNode {
		mapSet(on, "workflow_dispatch", cloneNode(baseDispatch))
		return
	}
	baseInputs := mapGet(baseDispatch, "inputs")
	inputs := mapGet(dispatch, "inputs")
	if inputs == nil || inputs.Kind != yaml.MappingNode {
		mapSet(dispatch, "inputs", cloneNode(baseInputs))
		return
	}
	if mapGet(inputs, "image_tag") == nil {
		mapSet(inputs, "image_tag", cloneNode(mapGet(baseInputs, "image_tag")))
	}
}

func stepID(step *yaml.Node) string {
	if step.Kind != yaml.MappingNode {
		return ""
	}
	id := mapGet(step, "id")
	if id == nil || id.Kind != yaml.ScalarNode {
		return ""
	}
	return id.Value
}

func findStep(steps *yaml.Node, id string) int {
	for i, step := range steps.Content {
		if stepID(step) == id {
			return i
		}
	}
	return -1
}

func documentRoot(document *yaml.Node) *yaml.Node {
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		return document.Content[0]
	}
	return nil
}

func mappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func mapGet(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func mapSet(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping.Kind != yaml.MappingNode {
		mapping.Kind = yaml.MappingNode
		mapping.Tag = "!!map"
		mapping.Content = nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

func ensureMapping(node *yaml.Node) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func cloneNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		clone.Content[i] = cloneNode(child)
	}
	return &clone
}

func marshalDocument(document *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode workflow: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close workflow encoder: %w", err)
	}
	out := buf.Bytes()
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return out, nil
}

// WorkflowPlan is a proposed merge of payload settings into a workflow file.
type WorkflowPlan struct {
	Path     string
	Existing []byte
	Updated  []byte
	Managed  bool
	Warnings []string
}

// Changed reports whether writing the plan would modify the file.
func (p WorkflowPlan) Changed() bool {
	return !bytes.Equal(p.Existing, p.Updated)
}

// Diff renders a unified diff between the current and proposed file.
func (p WorkflowPlan) Diff() string {
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(p.Existing)),
		B:        difflib.SplitLines(string(p.Updated)),
		FromFile: p.Path + " (current)",
		ToFile:   p.Path + " (proposed)",
		Context:  3,
	})
	if err != nil {
		return ""
	}
	return diff
}

// Write installs the plan. The first merge into an unmanaged file keeps a
// .bak copy next to it.
func (p WorkflowPlan) Write() error {
	if !p.Managed && len(p.Existing) > 0 {
		backup := p.Path + ".bak"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := writeFileAtomic(backup, p.Existing, 0o644); err != nil {
				return fmt.Errorf("write workflow backup: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("inspect workflow backup: %w", err)
		}
	}
	return writeFileAtomic(p.Path, p.Updated, 0o644)
}

// PlanWorkflow computes the merge without writing anything.
func PlanWorkflow(path string, opts domain.ProjectOptions) (WorkflowPlan, error) {
	// #nosec G304 -- path is the repo-local workflow path.
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return WorkflowPlan{}, fmt.Errorf("read workflow: %w", readErr)
	}
	updated, warnings, err := updateWorkflow(existing, opts)
	if err != nil {
		return WorkflowPlan{}, err
	}
	return WorkflowPlan{
		Path:     path,
		Existing: existing,
		Updated:  updated,
		Managed:  isManagedWorkflow(existing),
		Warnings: warnings,
	}, nil
}

func isManagedWorkflow(existing []byte) bool {
	return bytes.Contains(existing, []byte("id: payload-")) || bytes.Contains(existing, []byte(WorkflowMarker))
}

// EnsureWorkflow plans and writes a merge. Callers that need consent for
// unmanaged files should use PlanWorkflow instead.
func EnsureWorkflow(path string, opts domain.ProjectOptions) (changed bool, err error) {
	plan, err := PlanWorkflow(path, opts)
	if err != nil {
		return false, err
	}
	if !plan.Changed() {
		return false, nil
	}
	return true, plan.Write()
}

// FindWorkflow returns the managed workflow path: an existing file that
// contains a payload-* step, otherwise the default path.
func FindWorkflow(dir string) string {
	defaultPath := filepath.Join(dir, ".github", "workflows", "deploy.yml")
	entries, err := os.ReadDir(filepath.Join(dir, ".github", "workflows"))
	if err != nil {
		return defaultPath
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(dir, ".github", "workflows", name)
		// #nosec G304 -- path is under the repo's .github/workflows.
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("id: payload-")) {
			return path
		}
	}
	return defaultPath
}
