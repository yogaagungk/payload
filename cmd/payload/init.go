package main

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yogaagungk/payload/internal/config"
	"github.com/yogaagungk/payload/internal/domain"
	"github.com/yogaagungk/payload/internal/service"
)

func (a *app) initCmd() *cobra.Command {
	var (
		stackFlag  string
		portFlag   int
		appFlag    string
		healthFlag string
		dir        string
		workflow   string
		yes        bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate or update Dockerfile and the deploy workflow",
		RunE: func(_ *cobra.Command, _ []string) error {
			interactive := a.interactive && !yes
			reader := bufio.NewReader(a.stdin)

			stack, err := resolveStack(stackFlag, dir, interactive, reader, a.stdout)
			if err != nil {
				return err
			}

			port := portFlag
			if port == 0 {
				port = stack.DefaultPort()
				if interactive {
					value, err := prompt(reader, a.stdout, "Port", strconv.Itoa(port))
					if err != nil {
						return err
					}
					port, err = config.ParsePort(value)
					if err != nil {
						return err
					}
				}
			}
			if err := config.ValidatePort(port); err != nil {
				return err
			}

			appName := appFlag
			if appName == "" {
				appName = config.DefaultAppName(dir)
				if interactive {
					appName, err = prompt(reader, a.stdout, "App name", appName)
					if err != nil {
						return err
					}
				}
			}
			if err := config.ValidateAppName(appName); err != nil {
				return err
			}

			healthPath := healthFlag
			if healthPath == "" {
				healthPath = config.DefaultHealthPath
				if interactive {
					healthPath, err = prompt(reader, a.stdout, "Health path", healthPath)
					if err != nil {
						return err
					}
				}
			}
			if err := config.ValidateHealthPath(healthPath); err != nil {
				return err
			}

			opts := domain.ProjectOptions{
				AppName:    appName,
				Stack:      stack,
				Port:       port,
				HealthPath: healthPath,
			}

			dockerfilePath, created, err := service.EnsureDockerfile(dir, opts)
			if err != nil {
				return err
			}

			workflowPath := workflow
			if workflowPath == "" {
				workflowPath = service.FindWorkflow(dir)
			} else if !filepath.IsAbs(workflowPath) {
				workflowPath = filepath.Join(dir, workflowPath)
			}
			changed, err := service.EnsureWorkflow(workflowPath, opts)
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(a.stdout, "Initialized payload for %s (%s, port %d)\n", appName, stack, port)
			if created {
				_, _ = fmt.Fprintf(a.stdout, "  Dockerfile: created %s\n", dockerfilePath)
			} else {
				_, _ = fmt.Fprintf(a.stdout, "  Dockerfile: kept existing %s\n", dockerfilePath)
			}
			status := "unchanged"
			if changed {
				status = "created/updated"
			}
			_, _ = fmt.Fprintf(a.stdout, "  workflow: %s (%s)\n", workflowPath, status)
			_, _ = fmt.Fprintln(a.stdout, "  next: commit these files and set DEPLOY_* secrets (for example with blackbox sync)")
			return nil
		},
	}

	cmd.Flags().StringVar(&stackFlag, "stack", "", "tech stack: go, node, python or static (default: detect)")
	cmd.Flags().IntVar(&portFlag, "port", 0, "host port (default: 3000, 8080 for static)")
	cmd.Flags().StringVar(&appFlag, "app", "", "app/container name (default: directory name)")
	cmd.Flags().StringVar(&healthFlag, "health-path", "", "HTTP path for the post-deploy health check (default: /)")
	cmd.Flags().StringVar(&dir, "dir", ".", "project directory")
	cmd.Flags().StringVar(&workflow, "workflow", "", "workflow path (default: existing payload-managed file or .github/workflows/deploy.yml)")
	cmd.Flags().BoolVar(&yes, "yes", false, "use defaults and never prompt")
	return cmd
}

func resolveStack(flag, dir string, interactive bool, reader *bufio.Reader, out io.Writer) (domain.Stack, error) {
	if flag != "" {
		stack := domain.Stack(strings.ToLower(strings.TrimSpace(flag)))
		if !stack.Valid() {
			return "", fmt.Errorf("unsupported stack %q (use go, node, python or static)", flag)
		}
		return stack, nil
	}

	if detected := service.DetectStack(dir); detected != "" {
		return detected, nil
	}
	if !interactive {
		return "", fmt.Errorf("cannot detect the stack; pass --stack go|node|python|static")
	}

	_, _ = fmt.Fprintln(out, "Select stack:")
	for i, stack := range domain.AllStacks {
		_, _ = fmt.Fprintf(out, "  %d) %s\n", i+1, stack)
	}
	line, err := prompt(reader, out, "Stack", "1")
	if err != nil {
		return "", err
	}
	if number, err := strconv.Atoi(line); err == nil {
		if number < 1 || number > len(domain.AllStacks) {
			return "", fmt.Errorf("stack choice %d is out of range", number)
		}
		return domain.AllStacks[number-1], nil
	}
	stack := domain.Stack(strings.ToLower(line))
	if !stack.Valid() {
		return "", fmt.Errorf("unsupported stack %q", line)
	}
	return stack, nil
}

func prompt(reader *bufio.Reader, out io.Writer, label, fallback string) (string, error) {
	_, _ = fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return fallback, nil
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return fallback, nil
	}
	return line, nil
}
