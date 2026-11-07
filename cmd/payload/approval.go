package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yogaagungk/payload/internal/service"
)

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) ExitCode() int { return e.code }

func (a *app) shipWithApproval(ctx context.Context, opts service.ShipOptions, houstonURL, message string, approvalTimeout time.Duration) error {
	base := strings.TrimSpace(houstonURL)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("HOUSTON_URL"))
	}
	if base == "" {
		return errors.New("--houston-url or HOUSTON_URL is required with --require-approval")
	}
	validated, err := service.ValidateAPIURL(base)
	if err != nil {
		return fmt.Errorf("houston URL: %w", err)
	}
	token := strings.TrimSpace(os.Getenv("HOUSTON_WEBHOOK_TOKEN"))
	if token == "" {
		return errors.New("HOUSTON_WEBHOOK_TOKEN is required with --require-approval")
	}

	known := map[int64]bool{}
	if existing, err := opts.Client.ListRuns(ctx, opts.Owner, opts.Repo, opts.Ref); err == nil {
		for _, run := range existing {
			known[run.ID] = true
		}
	}

	requestedBy := strings.TrimSpace(os.Getenv("GIT_AUTHOR_NAME"))
	if requestedBy == "" {
		requestedBy = strings.TrimSpace(os.Getenv("USER"))
	}
	client := &service.HoustonClient{BaseURL: validated, Token: token}
	release, created, err := client.CreateRelease(ctx, service.ReleaseApprovalRequest{
		Repo: opts.Owner + "/" + opts.Repo, Ref: opts.Ref, SHA: opts.HeadSHA, Workflow: opts.Workflow,
		Inputs: opts.Inputs, RequestedBy: requestedBy, Message: message,
	})
	if err != nil {
		return err
	}
	if created {
		_, _ = fmt.Fprintf(a.stdout, "queued release %s (waiting for approval in houston; expires %s)\n",
			release.ID, release.ExpiresAt.Format("2006-01-02 15:04 UTC"))
	} else {
		_, _ = fmt.Fprintf(a.stdout, "existing pending release %s (expires %s)\n",
			release.ID, release.ExpiresAt.Format("2006-01-02 15:04 UTC"))
	}

	approvalCtx, cancel := context.WithTimeout(ctx, approvalTimeout)
	defer cancel()
	lastStatus := release.Status
	failures := 0
	for {
		if code, msg, terminal := terminalRelease(release); terminal {
			return &exitError{code: code, msg: msg}
		}
		switch release.Status {
		case "dispatched":
			return a.followApprovedRelease(ctx, opts, release, known)
		case "pending", "approved":
		default:
			return fmt.Errorf("release %s has unknown status %q", release.ID, release.Status)
		}
		if release.Status != lastStatus {
			lastStatus = release.Status
			_, _ = fmt.Fprintf(a.stdout, "release %s: %s\n", release.ID, release.Status)
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-approvalCtx.Done():
			timer.Stop()
			return &exitError{code: 6, msg: fmt.Sprintf("timed out waiting for release %s", release.ID)}
		case <-timer.C:
		}
		release, err = client.GetRelease(approvalCtx, release.ID)
		if err != nil {
			failures++
			var apiErr *service.HoustonError
			retryable := !errors.As(err, &apiErr) || apiErr.Status == 429 || apiErr.Status >= 500
			if !retryable || failures >= 5 {
				return fmt.Errorf("polling release %s: %w", release.ID, err)
			}
			delay := time.Duration(failures) * 200 * time.Millisecond
			if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
				delay = apiErr.RetryAfter
			}
			timer := time.NewTimer(delay)
			select {
			case <-approvalCtx.Done():
				timer.Stop()
				return &exitError{code: 6, msg: fmt.Sprintf("timed out waiting for release %s", release.ID)}
			case <-timer.C:
			}
			continue
		}
		failures = 0
	}
}

// terminalRelease maps terminal houston statuses to CLI exit codes.
func terminalRelease(release service.HoustonRelease) (code int, message string, terminal bool) {
	switch release.Status {
	case "rejected":
		return 3, fmt.Sprintf("release %s rejected", release.ID), true
	case "expired":
		return 4, fmt.Sprintf("release %s expired", release.ID), true
	case "dispatch_failed":
		reason := "dispatch failed"
		if release.Error != nil {
			reason = *release.Error
		}
		return 5, fmt.Sprintf("release %s: %s", release.ID, reason), true
	default:
		return 0, "", false
	}
}

func (a *app) followApprovedRelease(ctx context.Context, opts service.ShipOptions, release service.HoustonRelease, known map[int64]bool) error {
	opts.Reporter = service.NewPlainReporter(a.stdout)

	if release.RunID != nil {
		run, err := opts.Client.GetRun(ctx, opts.Owner, opts.Repo, *release.RunID)
		if err != nil {
			return err
		}
		final, err := service.FollowRun(ctx, opts, run)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.stdout, "run %d finished: %s (%s)\n", final.ID, final.Conclusion, final.HTMLURL)
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	for {
		runs, err := opts.Client.ListRuns(ctx, opts.Owner, opts.Repo, opts.Ref)
		if err == nil {
			var found []service.WorkflowRun
			for _, run := range runs {
				if known[run.ID] || run.HeadSHA != opts.HeadSHA {
					continue
				}
				if run.CreatedAt.Before(release.CreatedAt.Add(-time.Minute)) {
					continue
				}
				found = append(found, run)
			}
			if len(found) > 0 {
				sort.Slice(found, func(i, j int) bool { return found[i].ID < found[j].ID })
				final, followErr := service.FollowRun(ctx, opts, found[0])
				if followErr != nil {
					return followErr
				}
				_, _ = fmt.Fprintf(a.stdout, "run %d finished: %s (%s)\n", final.ID, final.Conclusion, final.HTMLURL)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return &exitError{code: 6, msg: "release dispatched but no workflow run was found"}
		case <-time.After(opts.PollInterval):
		}
	}
}
