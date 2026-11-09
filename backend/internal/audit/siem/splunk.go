package siem

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/secrets"
)

// splunkSender sends events to a Splunk HTTP Event Collector: a request per batch to
// /services/collector/event, its events one JSON object after the other, authenticated with the
// HEC token (Authorization: Splunk <token>). HEC answers 200 when it took them; Quai does not ask
// for indexer acknowledgment (open question 41).
type splunkSender struct {
	url        string
	token      string // a secret reference
	index      string
	sourcetype string
	hostname   string
	client     *http.Client
	secrets    *secrets.Store
}

// Source is the events' source.
const Source = "quai"

func newSplunk(c config.SplunkExport, client *http.Client, sec *secrets.Store, hostname string) *splunkSender {
	return &splunkSender{url: strings.TrimSuffix(c.URL, "/") + "/services/collector/event", token: c.Token.Ref, index: c.Index,
		sourcetype: c.Sourcetype, hostname: hostname, client: client, secrets: sec}
}

// hecEvent is an event as HEC takes it.
type hecEvent struct {
	Time       json.Number     `json:"time"`
	Host       string          `json:"host,omitempty"`
	Source     string          `json:"source"`
	Sourcetype string          `json:"sourcetype,omitempty"`
	Index      string          `json:"index,omitempty"`
	Event      json.RawMessage `json:"event"`
}

// hecReply is HEC's answer.
type hecReply struct {
	Text string `json:"text"`
	Code int    `json:"code"`
}

func (s *splunkSender) send(ctx context.Context, records []audit.Record) error {
	var body bytes.Buffer
	for _, r := range records {
		j, err := recordJSON(r)
		if err != nil {
			return err
		}
		at, err := time.Parse(audit.AtFormat, r.At)
		if err != nil {
			return err
		}
		ev, err := json.Marshal(hecEvent{Time: json.Number(fmt.Sprintf("%d.%03d", at.Unix(), at.Nanosecond()/int(time.Millisecond))),
			Host: s.hostname, Source: Source, Sourcetype: s.sourcetype, Index: s.index, Event: j})
		if err != nil {
			return err
		}
		body.Write(ev)
		body.WriteByte('\n')
	}
	token, err := s.secrets.Resolve(ctx, s.token)
	if err != nil {
		return fmt.Errorf("the HEC token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Splunk "+token.Reveal())
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var reply hecReply
	_ = json.Unmarshal(b, &reply)
	if resp.StatusCode != http.StatusOK {
		if reply.Text != "" {
			return fmt.Errorf("HEC answered %s: %q (code %d)", resp.Status, reply.Text, reply.Code)
		}
		return fmt.Errorf("HEC answered %s", resp.Status)
	}
	return nil
}
