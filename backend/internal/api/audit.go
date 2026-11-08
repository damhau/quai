package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
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
}

func registerAudit(a huma.API, s *store.Store) {
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
		return &out[AuditPageView]{Body: page}, nil
	})
}
