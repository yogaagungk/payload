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
		target   string
		workflow string
		repo     string
		apiURL   string
		remote   string
		inputs   []string
		plain    bool
		noPush   bool
		timeout  time.Duration
		poll     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Push, dispatch the deploy workflow and stream its logs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			git := service.Git{Dir: "."}

			branch := target
			if branch == "" {
				current, err := git.CurrentBranch(ctx)
				if err != nil {
					return fmt.Errorf("resolve current branch: %w", err)
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

			if !noPush {
				if err := git.Push(ctx, remote, branch); err != nil {
					return err
				}
			}

			token, err := a.githubToken()
			if err != nil {
				return err
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
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "maximum time to wait for the run")
	cmd.Flags().DurationVar(&poll, "poll", 2*time.Second, "status polling interval")
	return cmd
}

func (a *app) runShipTUI(ctx context.Context, opts service.ShipOptions) error {
	program := tea.NewProgram(tui.New(), tea.WithAltScreen())
	type result struct {
		run service.WorkflowRun
		err error
	}
	results := make(chan result, 1)

	go func() {
		opts.Reporter = tui.ProgramReporter{Program: program}
		run, err := service.Ship(ctx, opts)
		results <- result{run: run, err: err}
		program.Send(tui.DoneMsg{Run: run, Err: err})
	}()

	finalModel, err := program.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	res := <-results
	if model, ok := finalModel.(tui.Model); ok && res.run.ID == 0 {
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
