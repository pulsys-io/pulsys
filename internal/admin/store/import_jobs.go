// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/pulsys-io/pulsys/internal/importmsg"
)

const ImportJobTypeHFCacheImport = "hf_cache_import"

type ImportJobStatus string

const (
	ImportJobQueued  ImportJobStatus = "queued"
	ImportJobRunning ImportJobStatus = "running"
	// ImportJobRetrying is what we surface for River's Retryable
	// state: the previous attempt errored and the worker is waiting
	// out a backoff before the next attempt.  We split this from
	// Running because (a) the row is not currently being executed
	// so River.JobDelete will accept it, and (b) the admin UI
	// needs to expose a Delete action -- a job that has failed
	// many times in a row is the most common reason to remove a
	// row from the queue.
	ImportJobRetrying  ImportJobStatus = "retrying"
	ImportJobSucceeded ImportJobStatus = "succeeded"
	ImportJobFailed    ImportJobStatus = "failed"
	ImportJobCanceled  ImportJobStatus = "canceled"
)

type ImportJob struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// CreatedBy is the user/PAT subject that enqueued the job.
	CreatedBy *string         `json:"created_by,omitempty"`
	Type      string          `json:"type"`
	Status    ImportJobStatus `json:"status"`
	Payload   json.RawMessage `json:"payload"`
	Progress  json.RawMessage `json:"progress"`
	// Error is a short user-facing title for the most recent failure
	// (e.g. "Import timed out"). Nil for healthy jobs and for jobs
	// that were canceled by the user -- those are signaled solely by
	// Status so the UI can avoid styling them as critical.
	Error *string `json:"error,omitempty"`
	// ErrorHint is one sentence of plain-English next-step copy that
	// pairs with Error. Kept separate so the UI can lay it out below
	// the title with muted styling, matching Apple HIG alert structure.
	ErrorHint *string `json:"error_hint,omitempty"`
	// ErrorDetail is the raw error from the worker, surfaced behind a
	// "Technical details" disclosure for operators. Populated whenever
	// Error is populated; the two are never coupled by formatting.
	ErrorDetail *string `json:"error_detail,omitempty"`
	// CancelRequestedAt is set when an operator has called Cancel but
	// the row has not yet moved to a terminal state -- River signals
	// the worker's context and updates this metadata key, but the
	// row's state stays "running" until the worker returns. The UI
	// reads this to switch the row into a "Canceling..." state and
	// to surface the Force-remove escape hatch when a cancel signal
	// has been outstanding for too long.
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	Attempt           int        `json:"attempt"`
	LeaseOwner        *string    `json:"lease_owner,omitempty"`
	LeaseUntil        *time.Time `json:"lease_until,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

var (
	ErrImportJobNotFound = errors.New("import job not found")
	// ErrImportJobRunning is returned by DeleteImportJob when the
	// caller asks us to drop a job row that is still running. River's
	// JobDelete refuses to remove a job in the Running state because
	// the worker may still be mutating its row; the admin contract
	// is "cancel first, then delete", which the UI enforces by
	// hiding the delete affordance for queued/running jobs and
	// showing Cancel instead.
	ErrImportJobRunning = errors.New("import job is running; cancel before delete")
)

type importJobMeta struct {
	TenantID  string  `json:"tenant_id"`
	CreatedBy *string `json:"created_by,omitempty"`
}

type cacheImportArgs struct {
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision"`
	RepoType string `json:"repo_type"`
}

func (cacheImportArgs) Kind() string { return ImportJobTypeHFCacheImport }

// SetRiverClient wires the shared River client used for import job enqueue/list.
func (s *AdminStore) SetRiverClient(c *river.Client[pgx.Tx]) {
	s.river = c
}

// HasRiver reports whether import APIs are available.
func (s *AdminStore) HasRiver() bool {
	return s != nil && s.river != nil
}

func (s *AdminStore) CreateImportJob(ctx context.Context, tenantID, createdBy, typ string, payload json.RawMessage) (*ImportJob, error) {
	if s.river == nil {
		return nil, errors.New("river client not configured")
	}
	if typ == "" {
		typ = ImportJobTypeHFCacheImport
	}
	if typ != ImportJobTypeHFCacheImport {
		return nil, fmt.Errorf("unsupported import job type %q", typ)
	}
	spec, err := parseImportPayload(payload)
	if err != nil {
		return nil, err
	}
	meta := importJobMeta{TenantID: tenantID}
	if createdBy != "" {
		meta.CreatedBy = &createdBy
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	res, err := s.river.Insert(ctx, spec, &river.InsertOpts{Metadata: metaBytes})
	if err != nil {
		return nil, err
	}
	job, err := s.jobFromRow(res.Job, tenantID)
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (s *AdminStore) ListImportJobs(ctx context.Context, tenantID string, limit int) ([]ImportJob, error) {
	if s.river == nil {
		return nil, errors.New("river client not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	metaFilter, err := json.Marshal(map[string]string{"tenant_id": tenantID})
	if err != nil {
		return nil, err
	}
	res, err := s.river.JobList(ctx, river.NewJobListParams().
		Kinds(ImportJobTypeHFCacheImport).
		Metadata(string(metaFilter)).
		OrderBy(river.JobListOrderByTime, river.SortOrderDesc).
		First(limit))
	if err != nil {
		return nil, err
	}
	out := make([]ImportJob, 0, len(res.Jobs))
	for _, row := range res.Jobs {
		job, err := s.jobFromRow(row, tenantID)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []ImportJob{}
	}
	return out, nil
}

func (s *AdminStore) GetImportJob(ctx context.Context, tenantID, jobID string) (*ImportJob, error) {
	if s.river == nil {
		return nil, errors.New("river client not configured")
	}
	id, err := strconv.ParseInt(jobID, 10, 64)
	if err != nil {
		return nil, ErrImportJobNotFound
	}
	row, err := s.river.JobGet(ctx, id)
	if err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			return nil, ErrImportJobNotFound
		}
		return nil, err
	}
	if row.Kind != ImportJobTypeHFCacheImport {
		return nil, ErrImportJobNotFound
	}
	job, err := s.jobFromRow(row, tenantID)
	if err != nil {
		return nil, err
	}
	if job.TenantID != tenantID {
		return nil, ErrImportJobNotFound
	}
	return job, nil
}

// CancelImportJob asks River to cancel a tenant's import job.
//
// Tenant scoping: River's JobCancel is identified by the integer
// job id only -- it has no concept of our tenant_id metadata.  We
// therefore look the job up via GetImportJob first, which already
// rejects cross-tenant reads with ErrImportJobNotFound.  Without
// that pre-check a tenant could cancel another tenant's job by
// guessing IDs.
//
// Idempotency: calling JobCancel on an already-terminal job is a
// no-op in River and returns the row, not an error; we surface that
// to the caller as a successful 204 so retries from CI are safe.
func (s *AdminStore) CancelImportJob(ctx context.Context, tenantID, jobID string) error {
	if s == nil || s.river == nil {
		return errors.New("river client not configured")
	}
	if _, err := s.GetImportJob(ctx, tenantID, jobID); err != nil {
		return err
	}
	id, err := strconv.ParseInt(jobID, 10, 64)
	if err != nil {
		return ErrImportJobNotFound
	}
	if _, err := s.river.JobCancel(ctx, id); err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			return ErrImportJobNotFound
		}
		return err
	}
	return nil
}

// ForceCancelImportJob unsticks an import row that River won't move.
//
// Why this exists:
//
// River's JobCancel only signals a running worker via NOTIFY and sets
// metadata.cancel_attempted_at; the row's state stays "running" until
// the worker returns. When the worker process has died, hung in a
// blocking syscall, or never picked up the job, the row is effectively
// orphaned -- the regular Cancel returns 204 and audits cleanly, but
// the row never finalizes and DeleteImportJob refuses to remove it
// because it is still in the Running state. The admin UI is then
// stuck: Cancel does nothing visible, Delete is hidden.
//
// ForceCancelImportJob bypasses River's API and directly UPDATEs the
// river_job row to state='cancelled', finalized_at=now(). This is the
// "kill -9" escape hatch.
//
// Safety:
//
//   - If a worker is genuinely still alive, the worker's eventual
//     write through Completer.JobSetStateIfRunning is a no-op because
//     the IfRunning guard checks the current state at write time.
//     The worker may continue churning bytes briefly, but it can no
//     longer mutate the row's terminal state. JobUpdate writes to
//     metadata may continue; that is cosmetic, not corrupting.
//   - We do NOT remove the row -- the user can still see it in the
//     list with status=canceled and use Delete to remove it. This
//     keeps the audit trail intact (the row carries the original
//     error history and the operator's force_cancel_attempted_at
//     marker).
//
// Tenant scoping mirrors CancelImportJob: GetImportJob is the
// authoritative check; cross-tenant ids return ErrImportJobNotFound.
func (s *AdminStore) ForceCancelImportJob(ctx context.Context, tenantID, jobID string) error {
	if s == nil {
		return errors.New("admin store not configured")
	}
	if s.river == nil {
		return errors.New("river client not configured")
	}
	if s.Pool == nil {
		return errors.New("admin store has no database pool")
	}
	if _, err := s.GetImportJob(ctx, tenantID, jobID); err != nil {
		return err
	}
	id, err := strconv.ParseInt(jobID, 10, 64)
	if err != nil {
		return ErrImportJobNotFound
	}
	// Direct UPDATE. We jsonb_set both cancel_attempted_at (so River's
	// own bookkeeping reflects the cancel) and force_cancel_attempted_at
	// (so an operator scanning metadata can tell this row took the
	// escape hatch).
	//
	// COALESCE on finalized_at: if the row was already terminal (e.g.,
	// admin double-clicked Force-remove), keep the original finalized_at
	// so the row's history doesn't get rewritten on retry. The state
	// flip itself is also idempotent.
	cmd, err := s.Pool.Exec(ctx, `
		UPDATE river_job
		SET state = 'cancelled'::river_job_state,
		    finalized_at = COALESCE(finalized_at, now()),
		    metadata = jsonb_set(
		        jsonb_set(
		            COALESCE(metadata, '{}'::jsonb),
		            '{cancel_attempted_at}'::text[], to_jsonb(now()), true
		        ),
		        '{force_cancel_attempted_at}'::text[], to_jsonb(now()), true
		    )
		WHERE id = $1
	`, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrImportJobNotFound
	}
	return nil
}

// DeleteImportJob removes a terminal import job from the queue.
//
// Same tenant-scoping rationale as CancelImportJob.  We additionally
// refuse to delete a job in the Running state and return
// ErrImportJobRunning, which the handler maps to 409; the admin UI
// only exposes Delete for terminal states (succeeded/failed/canceled)
// so a 409 here usually means a race: the worker started between
// the UI's last poll and the click.
func (s *AdminStore) DeleteImportJob(ctx context.Context, tenantID, jobID string) error {
	if s == nil || s.river == nil {
		return errors.New("river client not configured")
	}
	job, err := s.GetImportJob(ctx, tenantID, jobID)
	if err != nil {
		return err
	}
	if job.Status == ImportJobRunning {
		return ErrImportJobRunning
	}
	id, err := strconv.ParseInt(jobID, 10, 64)
	if err != nil {
		return ErrImportJobNotFound
	}
	if _, err := s.river.JobDelete(ctx, id); err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			return ErrImportJobNotFound
		}
		// Race: the worker picked the job back up between our
		// status check above and this call.  River refuses to
		// delete a running row.  Surface the same 409 sentinel
		// so the admin UI can prompt the user to Cancel first.
		if errors.Is(err, rivertype.ErrJobRunning) {
			return ErrImportJobRunning
		}
		return err
	}
	return nil
}

func parseImportPayload(payload json.RawMessage) (cacheImportArgs, error) {
	var spec struct {
		RepoID   string `json:"repo_id"`
		Revision string `json:"revision"`
		RepoType string `json:"repo_type"`
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(payload, &spec); err != nil {
		return cacheImportArgs{}, err
	}
	spec.RepoID = strings.TrimSpace(spec.RepoID)
	spec.Revision = strings.TrimSpace(spec.Revision)
	spec.RepoType = strings.TrimSpace(spec.RepoType)
	if spec.RepoID == "" {
		return cacheImportArgs{}, errors.New("repo_id required")
	}
	if spec.Revision == "" {
		spec.Revision = "main"
	}
	if spec.RepoType == "" {
		spec.RepoType = "models"
	}
	return cacheImportArgs{
		RepoID:   spec.RepoID,
		Revision: spec.Revision,
		RepoType: spec.RepoType,
	}, nil
}

func (s *AdminStore) jobFromRow(row *rivertype.JobRow, expectTenant string) (*ImportJob, error) {
	if row == nil {
		return nil, ErrImportJobNotFound
	}
	var meta importJobMeta
	if len(row.Metadata) > 0 {
		_ = json.Unmarshal(row.Metadata, &meta)
	}
	if expectTenant != "" && meta.TenantID != expectTenant {
		return nil, ErrImportJobNotFound
	}
	payload := json.RawMessage(`{}`)
	if len(row.EncodedArgs) > 0 {
		var args cacheImportArgs
		if err := json.Unmarshal(row.EncodedArgs, &args); err != nil {
			return nil, err
		}
		var err error
		payload, err = json.Marshal(args)
		if err != nil {
			return nil, err
		}
	}
	progress := extractImportProgress(row.Metadata)
	cancelRequestedAt := extractCancelRequestedAt(row.Metadata)
	status := mapRiverImportState(row.State)
	var errMsg, errHint, errDetail *string
	// We render the error block only for actual failure surfaces.
	// Canceled jobs carry a JobCancelError row in row.Errors (River's
	// own bookkeeping), but the UI signals "canceled" via the status
	// badge alone; surfacing the cancel as an error would double up
	// and read as if the job had failed.
	if len(row.Errors) > 0 && status != ImportJobCanceled {
		raw := row.Errors[len(row.Errors)-1].Error
		msg := importmsg.HumanizeMessage(raw)
		if !msg.IsZero() {
			title := msg.Title
			errMsg = &title
			if msg.Hint != "" {
				hint := msg.Hint
				errHint = &hint
			}
			// Always surface the raw error in the disclosure for
			// operators; even when title happens to equal raw, the
			// formatting differs enough (mono, wrapped) to be useful.
			detail := raw
			errDetail = &detail
		}
	}
	updatedAt := row.CreatedAt
	if row.FinalizedAt != nil {
		updatedAt = *row.FinalizedAt
	} else if row.AttemptedAt != nil {
		updatedAt = *row.AttemptedAt
	}
	return &ImportJob{
		ID:                strconv.FormatInt(row.ID, 10),
		TenantID:          meta.TenantID,
		CreatedBy:         meta.CreatedBy,
		Type:              ImportJobTypeHFCacheImport,
		Status:            status,
		Payload:           payload,
		Progress:          progress,
		Error:             errMsg,
		ErrorHint:         errHint,
		ErrorDetail:       errDetail,
		CancelRequestedAt: cancelRequestedAt,
		Attempt:           row.Attempt,
		StartedAt:         row.AttemptedAt,
		CompletedAt:       row.FinalizedAt,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         updatedAt,
	}, nil
}

// extractCancelRequestedAt reads River's `cancel_attempted_at` from the
// job metadata. River writes this key whenever JobCancel is called on a
// running row; we expose it so the UI can show "Canceling..." and the
// force-remove escape hatch.
func extractCancelRequestedAt(metadata []byte) *time.Time {
	if len(metadata) == 0 {
		return nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return nil
	}
	raw, ok := meta["cancel_attempted_at"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	if t.IsZero() {
		return nil
	}
	return &t
}

func extractImportProgress(metadata []byte) json.RawMessage {
	if len(metadata) == 0 {
		return json.RawMessage(`{}`)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return json.RawMessage(`{}`)
	}
	if p, ok := meta["progress"]; ok && len(p) > 0 && string(p) != "null" {
		return p
	}
	if out, ok := meta[rivertype.MetadataKeyOutput]; ok && len(out) > 0 && string(out) != "null" {
		return out
	}
	return json.RawMessage(`{}`)
}

func mapRiverImportState(state rivertype.JobState) ImportJobStatus {
	switch state {
	case rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateScheduled:
		return ImportJobQueued
	case rivertype.JobStateRunning:
		return ImportJobRunning
	case rivertype.JobStateRetryable:
		return ImportJobRetrying
	case rivertype.JobStateCompleted:
		return ImportJobSucceeded
	case rivertype.JobStateCancelled:
		return ImportJobCanceled
	case rivertype.JobStateDiscarded:
		return ImportJobFailed
	default:
		return ImportJobQueued
	}
}
