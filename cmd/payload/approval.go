package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func (a *app) shipWithApproval(ctx context.Context, opts service.ShipOptions, houstonURL, message string) error {
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

	// Snapshot runs before queueing so the later lookup cannot pick an old run.
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
	release, err := client.CreateRelease(ctx, service.ReleaseApprovalRequest{
		Repo: opts.Owner + "/" + opts.Repo, Ref: opts.Ref, SHA: opts.HeadSHA, Workflow: opts.Workflow,
		Inputs: opts.Inputs, RequestedBy: requestedBy, Message: message,
	})
	if err != nil {
		return err
	}

	pollCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	for {
		switch release.Status {
		case "dispatched":
			return a.followApprovedRelease(pollCtx, opts, release, known)
		case "rejected":
			return &exitError{code: 3, msg: fmt.Sprintf("release %s rejected", release.ID)}
		case "expired":
			return &exitError{code: 4, msg: fmt.Sprintf("release %s expired", release.ID)}
		case "dispatch_failed":
			reason := "dispatch failed"
			if release.Error != nil {
				reason = *release.Error
			}
			return &exitError{code: 5, msg: fmt.Sprintf("release %s: %s", release.ID, reason)}
		case "pending", "approved":
		default:
			return fmt.Errorf("release %s has unknown status %q", release.ID, release.Status)
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return &exitError{code: 6, msg: fmt.Sprintf("timed out waiting for release %s", release.ID)}
		case <-timer.C:
		}
		release, err = client.GetRelease(pollCtx, release.ID)
		if err != nil {
			return err
		}
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

	// Houston left run_id NULL: find the new run ourselves.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
