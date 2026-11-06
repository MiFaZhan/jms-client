package mgmt

import (
	"context"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// Job paths.
//
// Verified 2026-11-06 on a live v4 with a caveat that matters: both views
// ANSWERED 403 with `{"detail":"命令执行已禁用"}`, not 404. The paths are
// therefore correct for this build and the feature is switched off on the
// instance. A 403 here is a configuration answer, not a bug, and the tool
// reports the server's own message instead of inventing one.
const (
	PathJobs          = "/api/v1/ops/jobs/"
	PathJobExecutions = "/api/v1/ops/job-executions/"
)

// Job is one job definition.
//
// Field names are v4 convention and unverified: the instance refused the
// read, so there was no sample to confirm against. Read-only, so a wrong
// name costs an empty column.
type Job struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	Creator     string `json:"creator_name,omitempty"`
	DateCreated string `json:"date_created,omitempty"`
	Comment     string `json:"comment,omitempty"`
}

// JobExecution is one job run.
type JobExecution struct {
	ID         string `json:"id"`
	Material   string `json:"material,omitempty"`
	JobType    string `json:"type,omitempty"`
	Creator    string `json:"creator_name,omitempty"`
	DateStart  string `json:"date_start,omitempty"`
	DateEnd    string `json:"date_finished,omitempty"`
	IsFinished bool   `json:"is_finished,omitempty"`
	IsSuccess  bool   `json:"is_success,omitempty"`
	TimeCost   any    `json:"time_cost,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
}

// JobQuery is one job search.
type JobQuery struct {
	Keyword string
	Since   string
	Until   string
	Limit   int
	Offset  int
}

// ListJobs returns one page of job definitions.
func ListJobs(ctx context.Context, c *api.Client, q JobQuery) (Page[Job], error) {
	hasLocal := q.Keyword != ""
	page, err := listPage[Job](ctx, c, PathJobs, ListOptions{
		Limit:   Window(q.Limit, hasLocal),
		Offset:  q.Offset,
		Filters: DateRange(nil, q.Since, q.Until),
	})
	if err != nil {
		return Page[Job]{}, err
	}
	kept, _ := FilterRows(page.Results, q.Limit, func(j Job) bool {
		return MatchKeyword(q.Keyword, j.Name, j.Type, j.Creator)
	})
	return Page[Job]{Count: page.Count, Results: kept}, nil
}

// ListJobExecutions returns one page of job executions.
func ListJobExecutions(ctx context.Context, c *api.Client, q JobQuery) (Page[JobExecution], error) {
	hasLocal := q.Keyword != ""
	page, err := listPage[JobExecution](ctx, c, PathJobExecutions, ListOptions{
		Limit:   Window(q.Limit, hasLocal),
		Offset:  q.Offset,
		Filters: DateRange(nil, q.Since, q.Until),
	})
	if err != nil {
		return Page[JobExecution]{}, err
	}
	kept, _ := FilterRows(page.Results, q.Limit, func(j JobExecution) bool {
		return MatchKeyword(q.Keyword, j.Material, j.JobType, j.Creator)
	})
	return Page[JobExecution]{Count: page.Count, Results: kept}, nil
}

// ListJobAudit returns the job records that live in the audit area.
//
// This is the fallback that works when the operational job views are
// disabled: job activity still appears in the audit log, so "what ran
// yesterday" is answerable even on an instance where /ops/jobs/ refuses.
func ListJobAudit(ctx context.Context, c *api.Client, q AuditQuery) (Page[AuditEntry], error) {
	q.Kind = AuditJob
	return ListAudit(ctx, c, q)
}
