package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
          install -m 700 -d "$HOME/.ssh"
          printf '%%s\n' "${DEPLOY_SSH_KEY}" > "$HOME/.ssh/id_ed25519"
          chmod 600 "$HOME/.ssh/id_ed25519"
          printf '%%s\n' "${DEPLOY_KNOWN_HOSTS}" > "$HOME/.ssh/known_hosts"
          chmod 600 "$HOME/.ssh/known_hosts"
          ssh -i "$HOME/.ssh/id_ed25519" -p "${DEPLOY_PORT}" \
            -o UserKnownHostsFile="$HOME/.ssh/known_hosts" \
            -o StrictHostKeyChecking=yes \
            "${DEPLOY_USER}@${DEPLOY_HOST}" \
            "APP_NAME='${APP_NAME}' APP_PORT='${APP_PORT}' CONTAINER_PORT='${CONTAINER_PORT}' HEALTH_PATH='${HEALTH_PATH}' IMAGE='${IMAGE}:${IMAGE_TAG}' GHCR_TOKEN='${GHCR_TOKEN}' GITHUB_ACTOR='${GITHUB_ACTOR}' bash -s" <<'EOSSH'
          set -euo pipefail
          echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
          docker pull "$IMAGE"
          NEXT="${APP_NAME}-next"
          OLD="${APP_NAME}-old"
          docker rm -f "$NEXT" >/dev/null 2>&1 || true
          docker run -d --name "$NEXT" --env-file "/var/lib/lander/env/${APP_NAME}.env" --restart unless-stopped -p "127.0.0.1:${APP_PORT}:${CONTAINER_PORT}" "$IMAGE"
          healthy=0
          for attempt in {1..30}; do
            if curl -fsS "http://127.0.0.1:${APP_PORT}${HEALTH_PATH}" >/dev/null 2>&1; then
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
          docker rename "$NEXT" "$APP_NAME"
          docker start "$APP_NAME" >/dev/null
          EOSSH
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
	if len(bytes.TrimSpace(existing)) == 0 {
		return WorkflowYAML(opts)
	}

	var current yaml.Node
	if err := yaml.Unmarshal(existing, &current); err != nil {
		return nil, fmt.Errorf("parse existing workflow: %w", err)
	}
	root := documentRoot(&current)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("existing workflow is not a YAML mapping")
	}

	baselineYAML, err := WorkflowYAML(opts)
	if err != nil {
		return nil, err
	}
	var baseline yaml.Node
	if err := yaml.Unmarshal(baselineYAML, &baseline); err != nil {
		return nil, fmt.Errorf("parse baseline workflow: %w", err)
	}
	baseRoot := documentRoot(&baseline)

	ensureManagedWorkflow(root, baseRoot)
	return marshalDocument(&current)
}

func ensureManagedWorkflow(root, base *yaml.Node) {
	if mapGet(root, "name") == nil {
		mapSet(root, "name", scalarNode("deploy"))
	}

	if on := mapGet(root, "on"); on == nil || on.Kind != yaml.MappingNode {
		mapSet(root, "on", cloneNode(mapGet(base, "on")))
	} else {
		for _, trigger := range []string{"push", "workflow_dispatch"} {
			if mapGet(on, trigger) == nil {
				mapSet(on, trigger, cloneNode(mapGet(mapGet(base, "on"), trigger)))
			}
		}
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

// EnsureWorkflow writes the merged workflow, creating parent directories.
func EnsureWorkflow(path string, opts domain.ProjectOptions) (changed bool, err error) {
	// #nosec G304 -- path is the repo-local workflow path.
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, fmt.Errorf("read workflow: %w", readErr)
	}
	updated, err := UpdateWorkflow(existing, opts)
	if err != nil {
		return false, err
	}
	if bytes.Equal(existing, updated) {
		return false, nil
	}
	if err := writeFileAtomic(path, updated, 0o644); err != nil {
		return false, err
	}
	return true, nil
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
