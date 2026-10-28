// Package ingest keeps Quai in step with the repositories (docs/design/01-repository-connections.md#sync):
// webhooks are the primary signal and polling the safety net. A webhook delivery is verified by
// the repository's connector, recorded once by its delivery ID, and processed as a job that
// re-reads what the events name from the platform: webhooks are notifications, not truth. A poll,
// every sync.pollInterval, fetches the repository, compares its head and reconciles Quai's open
// pull requests. Both record what the health page shows: the last webhook, fetch and poll.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Handler acts on a pull request of a repository, as re-read from the platform after a webhook
// or found by polling. Pull-request delivery (requests.Service) implements it.
type Handler interface {
	PullRequest(ctx context.Context, repo *repositories.Repo, pr connectors.PullRequest) error
	// Awaited returns the pull requests of the repository Quai waits on: a poll re-reads them
	// whatever their state, so a merge or a close without a webhook is seen.
	Awaited(ctx context.Context, repo *repositories.Repo) ([]string, error)
	// Reconcile reconciles what waits on the repository with what Git holds, after each fetch.
	Reconcile(ctx context.Context, repo *repositories.Repo) error
}

// Service ingests webhooks and polls repositories.
type Service struct {
	store   *store.Store
	reg     *repositories.Registry
	jobs    *jobs.Client
	handler Handler
	log     *slog.Logger
	now     func() time.Time
}

// New returns the service. Webhooks need a jobs client (UseJobs) to queue their processing.
func New(s *store.Store, reg *repositories.Registry, log *slog.Logger) *Service {
	return &Service{store: s, reg: reg, log: log, now: time.Now}
}

// UseJobs gives the service the client it queues jobs with.
func (s *Service) UseJobs(c *jobs.Client) { s.jobs = c }

// UseHandler gives the service what acts on pull requests; without one, they are only re-read.
func (s *Service) UseHandler(h Handler) { s.handler = h }

// MaxDelivery is the largest webhook body accepted.
const MaxDelivery = 4 << 20

// Webhooks returns the endpoint platforms deliver to: POST /webhooks/{repository}.
func (s *Service) Webhooks() http.Handler {
	r := chi.NewRouter()
	r.Post("/webhooks/{repository}", s.receive)
	return r
}

// Event is a normalized event, as a job carries it.
type Event struct {
	Push        *connectors.Push               `json:"push,omitempty"`
	PullRequest *connectors.PullRequestChanged `json:"pullRequest,omitempty"`
	Status      *connectors.StatusChanged      `json:"status,omitempty"`
	Policy      *connectors.PolicyChanged      `json:"policy,omitempty"`
	Run         *connectors.PipelineRunChanged `json:"run,omitempty"`
}

func wrap(e connectors.Event) Event {
	switch e := e.(type) {
	case connectors.Push:
		return Event{Push: &e}
	case connectors.PullRequestChanged:
		return Event{PullRequest: &e}
	case connectors.StatusChanged:
		return Event{Status: &e}
	case connectors.PolicyChanged:
		return Event{Policy: &e}
	case connectors.PipelineRunChanged:
		return Event{Run: &e}
	}
	return Event{}
}

func (s *Service) receive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "repository")
	repo, err := s.reg.Get(name)
	if err != nil || repo.Connector == nil || repo.Connector.Webhooks() == nil {
		problem(w, http.StatusNotFound, "no repository takes webhooks here")
		return
	}
	secret, ok, err := repo.WebhookSecret(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "webhook secret", slog.String("repository", name), slog.Any("error", err))
		problem(w, http.StatusServiceUnavailable, "the webhook secret cannot be read")
		return
	}
	if !ok {
		problem(w, http.StatusNotFound, "no repository takes webhooks here")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxDelivery))
	if err != nil {
		problem(w, http.StatusRequestEntityTooLarge, "the delivery is too large")
		return
	}
	d, err := repo.Connector.Webhooks().Parse(r.Header, body, secret)
	switch {
	case errors.Is(err, connectors.ErrUnauthenticated):
		s.log.WarnContext(ctx, "webhook refused: not authenticated", slog.String("repository", name))
		problem(w, http.StatusUnauthorized, "the delivery is not authenticated")
		return
	case err != nil:
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.RecordWebhook(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	args := WebhookArgs{Repository: name, Delivery: d.ID}
	for _, e := range d.Events {
		args.Events = append(args.Events, wrap(e))
	}
	duplicate := false
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if d.ID != "" {
			n, err := q.AddWebhookDelivery(ctx, db.AddWebhookDeliveryParams{Repository: name, DeliveryID: d.ID})
			if err != nil {
				return err
			}
			if duplicate = n == 0; duplicate {
				return nil
			}
		}
		if len(args.Events) == 0 {
			return nil
		}
		return jobs.InsertTx(ctx, s.jobs, tx, args, nil)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"delivery": d.ID, "events": len(args.Events), "duplicate": duplicate})
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "webhook", slog.String("path", r.URL.Path), slog.Any("error", err))
	problem(w, http.StatusInternalServerError, "the delivery could not be recorded; deliver it again")
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
}

// WebhookArgs processes a webhook delivery's events.
type WebhookArgs struct {
	Repository string  `json:"repository"`
	Delivery   string  `json:"delivery"`
	Events     []Event `json:"events"`
}

// Kind names the job.
func (WebhookArgs) Kind() string { return "process_webhook" }

// PollArgs polls one repository.
type PollArgs struct {
	Repository string `json:"repository"`
}

// Kind names the job.
func (PollArgs) Kind() string { return "poll_repository" }

// ScheduleArgs queues the polls that are due, and forgets old deliveries. It runs every minute.
type ScheduleArgs struct{}

// Kind names the job.
func (ScheduleArgs) Kind() string { return "schedule_polls" }

// Workers adds the service's workers.
func (s *Service) Workers(w *river.Workers) {
	river.AddWorker(w, &webhookWorker{s: s})
	river.AddWorker(w, &pollWorker{s: s})
	river.AddWorker(w, &scheduleWorker{s: s})
}

// Periodic is the job that schedules the polls, for jobs.Options.
func Periodic() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
		return ScheduleArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

// lockSnooze is how long a job waits when another holds the repository.
const lockSnooze = 5 * time.Second

type webhookWorker struct {
	river.WorkerDefaults[WebhookArgs]
	s *Service
}

func (w *webhookWorker) Work(ctx context.Context, job *river.Job[WebhookArgs]) error {
	return w.s.Process(ctx, job.Args.Repository, job.Args.Events)
}

// Process re-reads what events name: a push fetches the repository (its new head, its
// definitions), a pull request or a status on one is read again from the platform and handed to
// the Handler.
func (s *Service) Process(ctx context.Context, name string, events []Event) error {
	repo, err := s.reg.Get(name)
	if err != nil {
		s.log.WarnContext(ctx, "webhook for a repository that is gone", slog.String("repository", name))
		return nil
	}
	return jobs.WithRepositoryLock(ctx, s.store.Pool, name, lockSnooze, func(ctx context.Context) error {
		prs := map[string]bool{}
		fetch := false
		for _, e := range events {
			switch {
			case e.Push != nil:
				fetch = true
			case e.PullRequest != nil:
				prs[e.PullRequest.ID] = true
			case e.Status != nil && e.Status.PullRequest != "":
				prs[e.Status.PullRequest] = true
			case e.Policy != nil:
				s.log.InfoContext(ctx, "branch policy changed", slog.String("repository", name), slog.String("branch", e.Policy.Branch))
			}
		}
		if fetch {
			if err := s.fetch(ctx, repo); err != nil {
				return err
			}
		}
		for id := range prs {
			if err := s.reread(ctx, repo, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// fetch brings the repository's mirror and snapshot up to date, records it, and has the Handler
// reconcile what waits on the repository.
func (s *Service) fetch(ctx context.Context, repo *repositories.Repo) error {
	snap, err := repo.Snapshot(ctx, true)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", repo.Name, err)
	}
	if err := s.store.RecordFetch(ctx, db.RecordFetchParams{Repository: repo.Name, Head: snap.Commit}); err != nil {
		return err
	}
	if s.handler != nil {
		return s.handler.Reconcile(ctx, repo)
	}
	return nil
}

// reread reads a pull request from the platform and hands it to the Handler.
func (s *Service) reread(ctx context.Context, repo *repositories.Repo, id string) error {
	prs := repo.Connector.PullRequests()
	if prs == nil {
		return nil
	}
	pr, err := prs.Get(ctx, id)
	if errors.Is(err, connectors.ErrNotFound) {
		s.log.WarnContext(ctx, "webhook names a pull request that does not exist", slog.String("repository", repo.Name), slog.String("pull_request", id))
		return nil
	}
	if err != nil {
		return err
	}
	return s.handle(ctx, repo, pr)
}

func (s *Service) handle(ctx context.Context, repo *repositories.Repo, pr connectors.PullRequest) error {
	if s.handler == nil {
		return nil
	}
	return s.handler.PullRequest(ctx, repo, pr)
}

type pollWorker struct {
	river.WorkerDefaults[PollArgs]
	s *Service
}

func (w *pollWorker) Work(ctx context.Context, job *river.Job[PollArgs]) error {
	return w.s.Poll(ctx, job.Args.Repository)
}

// Poll fetches the repository, notes a head that moved without a webhook saying so, and hands
// Quai's open pull requests (under the branch prefix) to the Handler.
func (s *Service) Poll(ctx context.Context, name string) error {
	repo, err := s.reg.Get(name)
	if err != nil {
		return nil
	}
	return jobs.WithRepositoryLock(ctx, s.store.Pool, name, lockSnooze, func(ctx context.Context) error {
		before, err := s.store.GetRepositorySync(ctx, name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := s.store.RecordPoll(ctx, name); err != nil {
			return err
		}
		if err := s.fetch(ctx, repo); err != nil {
			return err
		}
		after, err := s.store.GetRepositorySync(ctx, name)
		if err != nil {
			return err
		}
		if before.Head != "" && before.Head != after.Head {
			s.log.InfoContext(ctx, "polling found a new head", slog.String("repository", name), slog.String("head", after.Head))
		}
		if err := s.checkTrust(ctx, repo); err != nil {
			s.log.WarnContext(ctx, "trust cannot be checked", slog.String("repository", name), slog.Any("error", err))
		}
		prs := repo.Connector.PullRequests()
		if prs == nil {
			return nil
		}
		open, err := prs.List(ctx, repo.Document.Spec.Delivery.BranchPrefix)
		if err != nil {
			return fmt.Errorf("pull requests of %s: %w", name, err)
		}
		seen := map[string]bool{}
		for _, pr := range open {
			seen[pr.ID] = true
			if err := s.handle(ctx, repo, pr); err != nil {
				return err
			}
		}
		if s.handler == nil {
			return nil
		}
		awaited, err := s.handler.Awaited(ctx, repo)
		if err != nil {
			return err
		}
		for _, id := range awaited {
			if !seen[id] {
				if err := s.reread(ctx, repo, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// checkTrust records whether the repository's definitions only change through review.
func (s *Service) checkTrust(ctx context.Context, repo *repositories.Repo) error {
	problems, err := repo.Trust(ctx)
	if err != nil {
		return err
	}
	if problems == nil {
		problems = []string{}
	}
	b, err := json.Marshal(problems)
	if err != nil {
		return err
	}
	return s.store.RecordTrust(ctx, db.RecordTrustParams{Repository: repo.Name, TrustProblems: b})
}

type scheduleWorker struct {
	river.WorkerDefaults[ScheduleArgs]
	s *Service
}

// DeliveryRetention is how long delivery IDs are kept against redeliveries.
const DeliveryRetention = 7 * 24 * time.Hour

// pending are the states of a poll job that is still to run: another one is not queued then.
var pending = []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
	rivertype.JobStateRetryable, rivertype.JobStateScheduled}

func (w *scheduleWorker) Work(ctx context.Context, _ *river.Job[ScheduleArgs]) error {
	s := w.s
	if _, err := s.store.DeleteWebhookDeliveriesBefore(ctx, pgtype.Timestamptz{Time: s.now().Add(-DeliveryRetention), Valid: true}); err != nil {
		return err
	}
	syncs, err := s.store.ListRepositorySync(ctx)
	if err != nil {
		return err
	}
	last := map[string]time.Time{}
	for _, r := range syncs {
		if r.LastPollAt.Valid {
			last[r.Repository] = r.LastPollAt.Time
		}
	}
	client := river.ClientFromContext[pgx.Tx](ctx)
	for _, repo := range s.reg.List() {
		interval := time.Duration(repo.Document.Spec.Sync.PollInterval)
		if t, ok := last[repo.Name]; ok && s.now().Sub(t) < interval {
			continue
		}
		if _, err := client.Insert(ctx, PollArgs{Repository: repo.Name}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: pending}}); err != nil {
			return err
		}
	}
	return nil
}
