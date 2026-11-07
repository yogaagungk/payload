package service

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	defaultPollInterval = 2 * time.Second
	defaultShipTimeout  = 30 * time.Minute
)

// Reporter receives run status and log updates while a deploy runs.
type Reporter interface {
	RunStarted(run WorkflowRun)
	Update(run WorkflowRun, jobs []Job)
	Log(jobName, chunk string)
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
		opts.Timeout = defaultShipTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	started := opts.Now()
	if err := opts.Client.Dispatch(ctx, opts.Owner, opts.Repo, opts.Workflow, opts.Ref, opts.Inputs); err != nil {
		return WorkflowRun{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	run, err := waitForRun(runCtx, opts, started)
	if err != nil {
		return run, err
	}
	if opts.Reporter != nil {
		opts.Reporter.RunStarted(run)
	}

	printed := map[int64]int{}
	for {
		jobs, err := opts.Client.ListJobs(runCtx, opts.Owner, opts.Repo, run.ID)
		if err != nil {
			return run, err
		}
		run, err = opts.Client.GetRun(runCtx, opts.Owner, opts.Repo, run.ID)
		if err != nil {
			return run, err
		}
		if opts.Reporter != nil {
			opts.Reporter.Update(run, jobs)
		}
		streamLogs(runCtx, opts, jobs, printed, run.Status == "completed")

		if run.Status == "completed" {
			// A final pass catches logs that only became available at the end.
			streamLogs(runCtx, opts, jobs, printed, true)
			if run.Conclusion != "success" {
				return run, fmt.Errorf("run %d completed with %q", run.ID, run.Conclusion)
			}
			return run, nil
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return run, fmt.Errorf("waiting for run %d: %w", run.ID, runCtx.Err())
		case <-timer.C:
		}
	}
}

func waitForRun(ctx context.Context, opts ShipOptions, since time.Time) (WorkflowRun, error) {
	for {
		runs, err := opts.Client.ListRuns(ctx, opts.Owner, opts.Repo, opts.Ref)
		if err != nil {
			return WorkflowRun{}, err
		}
		wantPath := workflowPath(opts.Workflow)
		var matches []WorkflowRun
		for _, run := range runs {
			if opts.HeadSHA != "" && run.HeadSHA != opts.HeadSHA {
				continue
			}
			if run.CreatedAt.Before(since.Add(-5 * time.Minute)) {
				continue
			}
			if wantPath != "" && run.Path != "" && run.Path != wantPath {
				continue
			}
			matches = append(matches, run)
		}
		if len(matches) > 0 {
			sort.Slice(matches, func(i, j int) bool { return matches[i].ID > matches[j].ID })
			return matches[0], nil
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

func workflowPath(workflow string) string {
	if workflow == "" {
		return ""
	}
	if strings.Contains(workflow, "/") {
		return workflow
	}
	return ".github/workflows/" + workflow
}

func streamLogs(ctx context.Context, opts ShipOptions, jobs []Job, printed map[int64]int, final bool) {
	if opts.Reporter == nil {
		return
	}
	for _, job := range jobs {
		if !final && job.Conclusion == "success" && printed[job.ID] > 0 {
			continue
		}
		logs, ready, err := opts.Client.JobLogs(ctx, opts.Owner, opts.Repo, job.ID)
		if err != nil || !ready {
			continue
		}
		if len(logs) <= printed[job.ID] {
			continue
		}
		opts.Reporter.Log(job.Name, logs[printed[job.ID]:])
		printed[job.ID] = len(logs)
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

// Log writes a log chunk prefixed with the job name.
func (p *PlainReporter) Log(jobName, chunk string) {
	_, _ = fmt.Fprintf(p.Out, "[%s] %s", jobName, chunk)
	if len(chunk) > 0 && chunk[len(chunk)-1] != '\n' {
		_, _ = fmt.Fprintln(p.Out)
	}
}
