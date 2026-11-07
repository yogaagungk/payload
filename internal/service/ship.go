package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	defaultPollInterval = 2 * time.Second
	defaultTimeout      = 30 * time.Minute
)

// Reporter receives run status and log updates while a deploy runs.
type Reporter interface {
	RunStarted(run WorkflowRun)
	Update(run WorkflowRun, jobs []Job)
	Log(jobName, chunk string)
}

// Warner is an optional reporter capability for actionable hints.
type Warner interface {
	Warn(message string)
}

// ShipOptions configures one workflow dispatch and its log stream.
type ShipOptions struct {
	Owner, Repo, Ref, Workflow, HeadSHA string
	Inputs                              map[string]string
	PollInterval, Timeout               time.Duration
	Client                              *GitHubClient
	Reporter                            Reporter
	Now                                 func() time.Time
}

// Ship dispatches the workflow and polls the run until it completes, passing
// status and log updates to the reporter.
func Ship(ctx context.Context, opts ShipOptions) (WorkflowRun, error) {
	if opts.Client == nil {
		return WorkflowRun{}, fmt.Errorf("github client is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	started := opts.Now()

	// Snapshot existing runs first: dispatch returns no run id, so only IDs
	// that appear after this snapshot can belong to this ship.
	existing, err := opts.Client.ListRuns(ctx, opts.Owner, opts.Repo, opts.Ref)
	if err != nil {
		return WorkflowRun{}, err
	}
	known := make(map[int64]bool, len(existing))
	for _, run := range existing {
		known[run.ID] = true
	}

	if err := opts.Client.Dispatch(ctx, opts.Owner, opts.Repo, opts.Workflow, opts.Ref, opts.Inputs); err != nil {
		return WorkflowRun{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	run, err := waitForRun(runCtx, opts, started, known)
	if err != nil {
		return run, err
	}
	if opts.Reporter != nil {
		opts.Reporter.RunStarted(run)
	}

	printed := map[int64]int{}
	finalFetched := map[int64]bool{}
	warned := false

	for {
		if !warned && opts.Now().Sub(started) >= time.Minute {
			warned = true
			if warner, ok := opts.Reporter.(Warner); ok {
				warner.Warn("no matching workflow_dispatch run yet; check that the pushed branch and commit are on the remote and that the workflow has a workflow_dispatch trigger")
			}
		}

		jobs, err := opts.Client.ListJobs(runCtx, opts.Owner, opts.Repo, run.ID)
		if err != nil {
			if waited, waitErr := pauseForRateLimit(runCtx, err); waited {
				if waitErr != nil {
					return run, waitErr
				}
				continue
			}
			return run, err
		}
		run, err = opts.Client.GetRun(runCtx, opts.Owner, opts.Repo, run.ID)
		if err != nil {
			if waited, waitErr := pauseForRateLimit(runCtx, err); waited {
				if waitErr != nil {
					return run, waitErr
				}
				continue
			}
			return run, err
		}
		if opts.Reporter != nil {
			opts.Reporter.Update(run, jobs)
		}
		streamLogs(runCtx, opts, jobs, printed, finalFetched, run.Status == "completed")

		if run.Status == "completed" {
			// A final pass catches logs that only became available at the end.
			streamLogs(runCtx, opts, jobs, printed, finalFetched, true)
			if run.Conclusion != "success" {
				return run, fmt.Errorf("run %d completed with %q", run.ID, run.Conclusion)
			}
			return run, nil
		}

		delay := pollDelay(opts.PollInterval, opts.Now().Sub(started))
		timer := time.NewTimer(delay)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return run, fmt.Errorf("waiting for run %d: %w", run.ID, runCtx.Err())
		case <-timer.C:
		}
	}
}

func waitForRun(ctx context.Context, opts ShipOptions, since time.Time, known map[int64]bool) (WorkflowRun, error) {
	warned := false
	for {
		runs, err := opts.Client.ListRuns(ctx, opts.Owner, opts.Repo, opts.Ref)
		if err != nil {
			if waited, waitErr := pauseForRateLimit(ctx, err); waited {
				if waitErr != nil {
					return WorkflowRun{}, waitErr
				}
				continue
			}
			return WorkflowRun{}, err
		}

		wantPath := workflowPath(opts.Workflow)
		var matches []WorkflowRun
		for _, run := range runs {
			if known[run.ID] {
				continue
			}
			if opts.HeadSHA != "" && run.HeadSHA != opts.HeadSHA {
				continue
			}
			if run.CreatedAt.Before(since.Add(-time.Minute)) {
				continue
			}
			if wantPath != "" && run.Path != "" && run.Path != wantPath {
				continue
			}
			matches = append(matches, run)
		}
		if len(matches) > 0 {
			sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
			return matches[0], nil
		}

		if !warned && opts.Now().Sub(since) >= time.Minute {
			warned = true
			if warner, ok := opts.Reporter.(Warner); ok {
				warner.Warn("no matching workflow_dispatch run yet; check that the pushed branch and commit are on the remote and that the workflow has a workflow_dispatch trigger")
			}
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return WorkflowRun{}, fmt.Errorf("waiting for the workflow run: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func pollDelay(base, elapsed time.Duration) time.Duration {
	switch {
	case elapsed >= 10*time.Minute:
		if base < 10*time.Second {
			return 10 * time.Second
		}
	case elapsed >= time.Minute:
		if base < 5*time.Second {
			return 5 * time.Second
		}
	}
	return base
}

func pauseForRateLimit(ctx context.Context, err error) (bool, error) {
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) {
		return false, nil
	}
	delay := rateErr.RetryAfter
	if delay < 10*time.Millisecond {
		delay = 10 * time.Millisecond
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return true, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}

func workflowPath(workflow string) string {
	if workflow == "" {
		return ""
	}
	if strings.Contains(workflow, "/") {
		return workflow
	}
	return ".github/workflows/" + workflow
}

func streamLogs(ctx context.Context, opts ShipOptions, jobs []Job, printed map[int64]int, finalFetched map[int64]bool, final bool) {
	if opts.Reporter == nil {
		return
	}
	for _, job := range jobs {
		if finalFetched[job.ID] {
			continue
		}
		if !final && job.Conclusion == "success" && printed[job.ID] > 0 {
			continue
		}
		logs, ready, err := opts.Client.JobLogs(ctx, opts.Owner, opts.Repo, job.ID)
		if err != nil || !ready {
			continue
		}
		if len(logs) > printed[job.ID] {
			opts.Reporter.Log(job.Name, logs[printed[job.ID]:])
			printed[job.ID] = len(logs)
		}
		if final || job.Conclusion != "" {
			finalFetched[job.ID] = true
		}
	}
}

// PlainReporter streams status transitions and logs as plain text.
type PlainReporter struct {
	Out        io.Writer
	runStatus  string
	jobStatus  map[int64]string
	stepStatus map[string]string
}

// NewPlainReporter builds a plain reporter writing to out.
func NewPlainReporter(out io.Writer) *PlainReporter {
	return &PlainReporter{
		Out:        out,
		jobStatus:  map[int64]string{},
		stepStatus: map[string]string{},
	}
}

// RunStarted reports the run URL.
func (p *PlainReporter) RunStarted(run WorkflowRun) {
	_, _ = fmt.Fprintf(p.Out, "run %d: %s\n", run.ID, run.HTMLURL)
}

// Update prints only status changes.
func (p *PlainReporter) Update(run WorkflowRun, jobs []Job) {
	if p.runStatus != run.Status {
		p.runStatus = run.Status
		_, _ = fmt.Fprintf(p.Out, "run: %s\n", run.Status)
	}
	for _, job := range jobs {
		if p.jobStatus[job.ID] != job.Status {
			p.jobStatus[job.ID] = job.Status
			_, _ = fmt.Fprintf(p.Out, "job %s: %s\n", job.Name, job.Status)
		}
		for _, step := range job.Steps {
			key := fmt.Sprintf("%d/%d", job.ID, step.Number)
			if p.stepStatus[key] != step.Status {
				p.stepStatus[key] = step.Status
				_, _ = fmt.Fprintf(p.Out, "  step %s: %s\n", step.Name, step.Status)
			}
		}
	}
}

// Log writes a sanitized log chunk prefixed with the job name.
func (p *PlainReporter) Log(jobName, chunk string) {
	chunk = SanitizeLog(chunk)
	_, _ = fmt.Fprintf(p.Out, "[%s] %s", jobName, chunk)
	if len(chunk) > 0 && chunk[len(chunk)-1] != '\n' {
		_, _ = fmt.Fprintln(p.Out)
	}
}

// Warn prints an actionable hint.
func (p *PlainReporter) Warn(message string) {
	_, _ = fmt.Fprintf(p.Out, "warning: %s\n", message)
}
