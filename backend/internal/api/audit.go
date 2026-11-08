package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/audit/keeper"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// AuditInput pages and filters the audit log.
type AuditInput struct {
	Before     int64     `query:"before" minimum:"0" doc:"Events before this seq: the next cursor of the page before; 0 or absent, from the newest"`
	Limit      int32     `query:"limit" minimum:"1" maximum:"500" default:"100"`
	Action     string    `query:"action" maxLength:"100" doc:"An action (request.approved), or its prefix (request)"`
	Actor      string    `query:"actor" maxLength:"500" doc:"Who acted: a person's name, a repository for its runner, quai for Quai itself"`
	ObjectType string    `query:"objectType" maxLength:"100" doc:"What kind of object: request, repository, token, user"`
	Object     string    `query:"object" maxLength:"500" doc:"What the event is about: a request ID, a resource, a repository, a token ID or a person's name"`
	Since      time.Time `query:"since" doc:"Events at or after this time"`
	Until      time.Time `query:"until" doc:"Events before this time"`
}

// AuditActorView is who did what an event records. Personal data is a pseudonym (pd:…), its
// clear value in the page's personal map until it is deleted.
type AuditActorView struct {
	Type     string `json:"type" enum:"user,token,runner,system"`
	ID       string `json:"id"`
	Token    string `json:"token,omitempty" doc:"The API token's ID"`
	IP       string `json:"ip,omitempty"`
	AuthTime string `json:"authTime,omitempty" doc:"When the person last authenticated with the identity provider"`
}

// AuditObjectView is what an event is about.
type AuditObjectView struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Resource string `json:"resource,omitempty"`
}

// AuditEventView is an event as recorded: its hash covers seq, at, actor, action, object and
// details, chained on prev (docs/design/09-security-and-compliance.md#audit-log).
type AuditEventView struct {
	Seq     int64           `json:"seq"`
	At      string          `json:"at"`
	Actor   AuditActorView  `json:"actor"`
	Action  string          `json:"action"`
	Object  AuditObjectView `json:"object"`
	Details map[string]any  `json:"details"`
	Prev    string          `json:"prev"`
	Hash    string          `json:"hash"`
}

// AuditPageView is a page of the audit log, newest first.
type AuditPageView struct {
	Events []AuditEventView `json:"events"`
	// Personal maps the pseudonyms in the events to their clear values, those not deleted.
	Personal map[string]string `json:"personal"`
	Next     int64             `json:"next,omitempty" doc:"Pass it as before for the next page; absent on the last one"`
	// Protected says whether the database keeps Quai from changing or deleting events: false when
	// the server connects as the schema's owner (open question 19).
	Protected bool `json:"protected"`
	// Checkpoint is the latest signed checkpoint; absent before the first, or without a key.
	Checkpoint *AuditCheckpointView `json:"checkpoint,omitempty"`
	// Verification is the outcome of the worker's last daily verification; absent before the first.
	Verification *AuditVerificationView `json:"verification,omitempty"`
}

// AuditCheckpointView is a signed checkpoint of the audit log: the event it covers.
type AuditCheckpointView struct {
	Seq int64  `json:"seq"`
	At  string `json:"at"`
	Key string `json:"key" doc:"The ID of the public key that verifies it: the SHA-256 of its 32 bytes, in hex"`
}

// AuditVerificationView is the outcome of a verification of the audit log, recorded as the
// event at seq.
type AuditVerificationView struct {
	Seq      int64  `json:"seq"`
	At       string `json:"at"`
	Verified bool   `json:"verified" doc:"Whether the chain, its checkpoints and the audit repository's latest checkpoint held"`
	Events   int64  `json:"events" doc:"How many events held"`
	Broken   int64  `json:"broken,omitempty" doc:"The first seq where the log fails"`
	Reason   string `json:"reason,omitempty" doc:"Why it fails there"`
}

// auditState adds the latest checkpoint and verification to a page.
func auditState(ctx context.Context, s *store.Store, page *AuditPageView) error {
	if c, err := s.LatestAuditCheckpoint(ctx); err == nil {
		page.Checkpoint = &AuditCheckpointView{Seq: c.Seq, At: audit.Time(c.At.Time), Key: c.Key}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	ev, err := s.LastAuditVerification(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var d struct {
		Events int64  `json:"events"`
		Seq    int64  `json:"seq"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Details, &d); err != nil {
		return err
	}
	v := &AuditVerificationView{Seq: ev.Seq, At: audit.Time(ev.At.Time), Verified: ev.Action == keeper.ActionVerified, Events: d.Events}
	if !v.Verified {
		v.Broken, v.Reason = d.Seq, d.Reason
	}
	page.Verification = v
	return nil
}

func registerAudit(a huma.API, s *store.Store, log *slog.Logger) {
	huma.Register(a, huma.Operation{
		OperationID: "list-audit-events", Method: http.MethodGet, Path: Prefix + "/audit",
		Summary: "Read the audit log (auditors and platform admins)", Tags: []string{"Audit"},
	}, func(ctx context.Context, in *AuditInput) (*out[AuditPageView], error) {
		if _, err := authorize(ctx, authz.ReadAudit, authz.Object{}); err != nil {
			return nil, err
		}
		p := db.ListAuditEventsParams{MaxRows: in.Limit}
		if in.Before > 0 {
			p.Before = pgtype.Int8{Int64: in.Before, Valid: true}
		}
		text := func(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }
		p.Action, p.Actor, p.ObjectType, p.Object = text(in.Action), text(in.Actor), text(in.ObjectType), text(in.Object)
		if !in.Since.IsZero() {
			p.Since = pgtype.Timestamptz{Time: in.Since, Valid: true}
		}
		if !in.Until.IsZero() {
			p.Until = pgtype.Timestamptz{Time: in.Until, Valid: true}
		}
		rows, err := s.ListAuditEvents(ctx, p)
		if err != nil {
			return nil, err
		}
		page := AuditPageView{Events: make([]AuditEventView, 0, len(rows)), Personal: map[string]string{}}
		seqs := make([]int64, len(rows))
		for i, r := range rows {
			v := AuditEventView{Seq: r.Seq, At: audit.Time(r.At.Time), Action: r.Action, Prev: r.Prev, Hash: r.Hash}
			if err := json.Unmarshal(r.Actor, &v.Actor); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(r.Object, &v.Object); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(r.Details, &v.Details); err != nil {
				return nil, err
			}
			page.Events = append(page.Events, v)
			seqs[i] = r.Seq
		}
		if len(rows) == int(in.Limit) {
			page.Next = rows[len(rows)-1].Seq
		}
		personal, err := s.AuditPersonalOf(ctx, seqs)
		if err != nil {
			return nil, err
		}
		for _, v := range personal {
			if audit.PersonalHolds(v.Hash, v.Salt, v.Value) {
				page.Personal[v.Hash] = v.Value
			}
		}
		protected, err := s.AuditProtected(ctx)
		if err != nil {
			return nil, err
		}
		page.Protected = protected.Bool
		if err := auditState(ctx, s, &page); err != nil {
			return nil, err
		}
		return &out[AuditPageView]{Body: page}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "export-audit-log", Method: http.MethodGet, Path: Prefix + "/audit/export",
		Summary: "Export the audit log, to verify it offline with quai audit verify (auditors and platform admins)",
		Description: "The log as it is at one moment, in JSON lines: its signed checkpoints, then its events in seq " +
			"order, with personal data as pseudonyms (docs/design/09-security-and-compliance.md#audit-log).",
		Tags: []string{"Audit"},
		Responses: map[string]*huma.Response{"200": {
			Description: "JSON lines: checkpoints, then events",
			Content:     map[string]*huma.MediaType{auditExportType: {Schema: &huma.Schema{Type: "string"}}},
		}},
	}, func(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		if _, err := authorize(ctx, authz.ReadAudit, authz.Object{}); err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			hc.SetHeader("Content-Type", auditExportType)
			hc.SetHeader("Content-Disposition", `attachment; filename="quai-audit.jsonl"`)
			hc.SetHeader("Cache-Control", "no-store")
			hc.SetStatus(http.StatusOK)
			if _, err := audit.Export(hc.Context(), s.Pool, hc.BodyWriter()); err != nil {
				// The status is sent: the export stops short, which verifying it shows when a line is
				// cut or a checkpoint is beyond its last event, and the log says why.
				log.Error("audit export", slog.Any("error", err))
			}
		}}, nil
	})
}

// auditExportType is the media type of an export of the audit log.
const auditExportType = "application/jsonl"
