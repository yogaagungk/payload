package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yogaagungk/payload/internal/service"
	"github.com/yogaagungk/payload/internal/tui"
)

func (a *app) shipCmd() *cobra.Command {
	var (
		target          string
		workflow        string
		repo            string
		apiURL          string
		remote          string
		inputs          []string
		plain           bool
		noPush          bool
		requireApproval bool
		houstonURL      string
		message         string
		approvalTimeout time.Duration
		timeout         time.Duration
		poll            time.Duration
	)

	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Push, dispatch the deploy workflow and stream its logs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			git := service.Git{Dir: "."}

			current, currentErr := git.CurrentBranch(ctx)
			branch := target
			if branch == "" {
				if currentErr != nil {
					return fmt.Errorf("resolve current branch: %w", currentErr)
				}
				branch = current
			}

			headSHA, err := git.HeadSHA(ctx)
			if err != nil {
				return err
			}

			owner, name, err := resolveShipRepo(repo, git, ctx)
			if err != nil {
				return err
			}
			baseURL, err := service.ValidateAPIURL(apiURL)
			if err != nil {
				return fmt.Errorf("--api: %w", err)
			}

			dispatchInputs, err := parseInputs(inputs)
			if err != nil {
				return err
			}

			token, err := a.githubToken()
			if err != nil {
				return err
			}

			if currentErr == nil && target != "" && target != current {
				_, _ = fmt.Fprintf(a.stderr, "warning: shipping HEAD of %s to %s\n", current, target)
			}
			if status, statusErr := git.Status(ctx); statusErr == nil && strings.TrimSpace(status) != "" {
				_, _ = fmt.Fprintln(a.stderr, "warning: working tree has uncommitted changes; only the pushed commit is deployed")
			}

			if !noPush {
				if err := git.Push(ctx, remote, branch); err != nil {
					return err
				}
			}
			opts := service.ShipOptions{
				Owner:        owner,
				Repo:         name,
				Ref:          branch,
				Workflow:     workflow,
				HeadSHA:      headSHA,
				Inputs:       dispatchInputs,
				PollInterval: poll,
				Timeout:      timeout,
				Client:       &service.GitHubClient{BaseURL: baseURL, Token: token},
			}

			if requireApproval {
				if !plain && writerIsTerminal(a.stdout) {
					_, _ = fmt.Fprintln(a.stderr, "note: approval mode streams plain text")
				}
				return a.shipWithApproval(ctx, opts, houstonURL, message, approvalTimeout)
			}

			if plain || !writerIsTerminal(a.stdout) {
				opts.Reporter = service.NewPlainReporter(a.stdout)
				run, err := service.Ship(ctx, opts)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(a.stdout, "run %d finished: %s (%s)\n", run.ID, run.Conclusion, run.HTMLURL)
				return nil
			}

			return a.runShipTUI(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&target, "target", "", "branch to push and dispatch (default: current branch)")
	cmd.Flags().StringVar(&workflow, "workflow", "deploy.yml", "workflow file name")
	cmd.Flags().StringVar(&repo, "repo", "", "repository as owner/name (default: origin remote)")
	cmd.Flags().StringVar(&apiURL, "api", "https://api.github.com", "GitHub API base URL (https, or http on loopback)")
	cmd.Flags().StringVar(&remote, "remote", "origin", "git remote to push to")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "workflow_dispatch input as key=value (repeatable)")
	cmd.Flags().BoolVar(&plain, "plain", false, "stream plain text instead of the TUI")
	cmd.Flags().BoolVar(&noPush, "no-push", false, "skip git push (the ref is already on the remote)")
	cmd.Flags().BoolVar(&requireApproval, "require-approval", false, "queue a houston approval instead of dispatching directly")
	cmd.Flags().StringVar(&houstonURL, "houston-url", "", "houston base URL (default HOUSTON_URL)")
	cmd.Flags().StringVar(&message, "message", "", "short release note shown in houston")
	cmd.Flags().DurationVar(&approvalTimeout, "approval-timeout", 24*time.Hour, "maximum time to wait for houston approval")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "maximum time to wait for the run")
	cmd.Flags().DurationVar(&poll, "poll", 2*time.Second, "status polling interval")
	return cmd
}

func (a *app) runShipTUI(ctx context.Context, opts service.ShipOptions) error {
	return a.runShipTUIWith(ctx, opts, func(model tui.Model) *tea.Program {
		return tea.NewProgram(model, tea.WithAltScreen())
	})
}

func (a *app) runShipTUIWith(ctx context.Context, opts service.ShipOptions, newProgram func(tui.Model) *tea.Program) error {
	shipCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	program := newProgram(tui.New())
	type result struct {
		run service.WorkflowRun
		err error
	}
	results := make(chan result, 1)

	go func() {
		opts.Reporter = tui.ProgramReporter{Program: program}
		run, err := service.Ship(shipCtx, opts)
		results <- result{run: run, err: err}
		program.Send(tui.DoneMsg{Run: run, Err: err})
	}()

	finalModel, err := program.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	model, _ := finalModel.(tui.Model)
	if !model.Done() {
		// Explicit detach (q/Esc/Ctrl+C): stop polling, leave the run alive.
		cancel()
		if run := model.Run(); run != nil && run.ID != 0 {
			_, _ = fmt.Fprintf(a.stdout, "detached: run %d continues on GitHub: %s\n", run.ID, run.HTMLURL)
		} else {
			_, _ = fmt.Fprintln(a.stdout, "detached; the workflow continues on GitHub")
		}
		return nil
	}

	res := <-results
	if res.run.ID == 0 {
		if run := model.Run(); run != nil {
			res.run = *run
		}
	}
	if res.err != nil {
		return res.err
	}
	_, _ = fmt.Fprintf(a.stdout, "run %d finished: %s (%s)\n", res.run.ID, res.run.Conclusion, res.run.HTMLURL)
	return nil
}

func resolveShipRepo(explicit string, git service.Git, ctx context.Context) (string, string, error) {
	if explicit != "" {
		return service.SplitRepo(explicit)
	}
	remoteURL, err := git.RemoteURL(ctx)
	if err != nil {
		return "", "", fmt.Errorf("cannot infer repository from origin remote; pass --repo owner/name")
	}
	return service.ParseRepoURL(remoteURL)
}

func parseInputs(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	inputs := make(map[string]string, len(specs))
	for _, spec := range specs {
		key, value, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--input must be key=value, got %q", spec)
		}
		inputs[strings.TrimSpace(key)] = value
	}
	return inputs, nil
}

func writerIsTerminal(w interface{ Write([]byte) (int, error) }) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// githubToken reads GITHUB_TOKEN or prompts like gh; the token is never a
// flag and is never echoed.
func (a *app) githubToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token, nil
	}
	if !a.interactive {
		line, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("GITHUB_TOKEN is not set and no token was provided on stdin")
		}
		token := strings.TrimSpace(line)
		if token == "" {
			return "", errors.New("GITHUB_TOKEN is not set and the stdin token was empty")
		}
		return token, nil
	}
	file, ok := a.stdin.(*os.File)
	if !ok {
		return "", errors.New("cannot prompt for a token without a terminal; set GITHUB_TOKEN")
	}
	_, _ = fmt.Fprint(a.stderr, "GitHub token: ")
	data, err := term.ReadPassword(int(file.Fd()))
	_, _ = fmt.Fprintln(a.stderr)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("empty GitHub token")
	}
	return token, nil
}
