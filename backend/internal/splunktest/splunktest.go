// Package splunktest is a fake Splunk HTTP Event Collector (HEC), for the tests of the audit log's
// export (docs/plan/phase-4-backlog.md#test-infrastructure): there is no Splunk account to test
// with. It implements only what Quai calls, POST /services/collector/event, over HTTPS with a
// certificate of its own, and keeps the events it took for the test to read.
//
// It follows Splunk's documentation, read on 2026-11-09:
//   - Format events for HTTP Event Collector
//     (https://help.splunk.com/en/splunk-enterprise/get-started/get-data-in/10.2/get-data-with-http-event-collector/format-events-for-http-event-collector):
//     the Authorization: Splunk <token> scheme; a request's events one JSON object after the
//     other, or in a JSON array; each with event and the optional time (epoch seconds, with
//     milliseconds after the point), host, source, sourcetype, index and fields; an index outside
//     the token's allowed ones is refused.
//   - Troubleshoot HTTP Event Collector, its "Possible error codes"
//     (https://help.splunk.com/en/splunk-enterprise/get-started/get-data-in/10.2/get-data-with-http-event-collector/troubleshoot-http-event-collector):
//     each answer's JSON body {"text", "code"}, and the HTTP status of each code. The fake uses
//     0 (Success), 1 (Token disabled), 2 (Token is required), 3 (Invalid authorization), 4
//     (Invalid token), 5 (No data), 6 (Invalid data format), 7 (Incorrect index), 9 (Server is
//     busy, 503), 12 (Event field is required), 13 (Event field cannot be blank) and 26 (HEC queue
//     is at capacity, 429).
//
// What the pages do not say, the fake assumes: a request refused takes none of its events
// (Splunk may have indexed those before the invalid one, which "invalid-event-number" names;
// Quai sends the whole batch again either way, so repeats are possible, never a loss); and no
// answer carries Retry-After, which the pages do not mention. It is not verified against a real
// Splunk.
package splunktest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Path is the endpoint of JSON events.
const Path = "/services/collector/event"

// Event is an event the collector took.
type Event struct {
	Time       json.Number     `json:"time"`
	Host       string          `json:"host"`
	Source     string          `json:"source"`
	Sourcetype string          `json:"sourcetype"`
	Index      string          `json:"index"`
	Event      json.RawMessage `json:"event"`
	Fields     map[string]any  `json:"fields"`
}

// Reply is the collector's answer: a status code of Splunk's table, and its text.
type Reply struct {
	Text  string `json:"text"`
	Code  int    `json:"code"`
	Event *int   `json:"invalid-event-number,omitempty"`
}

// The answers, by code, and their HTTP status.
var (
	Success           = Reply{Text: "Success", Code: 0}
	TokenDisabled     = Reply{Text: "Token disabled", Code: 1}
	TokenRequired     = Reply{Text: "Token is required", Code: 2}
	InvalidAuth       = Reply{Text: "Invalid authorization", Code: 3}
	InvalidToken      = Reply{Text: "Invalid token", Code: 4}
	NoData            = Reply{Text: "No data", Code: 5}
	InvalidDataFormat = Reply{Text: "Invalid data format", Code: 6}
	IncorrectIndex    = Reply{Text: "Incorrect index", Code: 7}
	ServerBusy        = Reply{Text: "Server is busy", Code: 9}
	EventRequired     = Reply{Text: "Event field is required", Code: 12}
	EventBlank        = Reply{Text: "Event field cannot be blank", Code: 13}
	QueueAtCapacity   = Reply{Text: "HEC queue is at capacity and cannot process any more requests", Code: 26}
)

var statuses = map[int]int{0: http.StatusOK, 1: http.StatusForbidden, 2: http.StatusUnauthorized, 3: http.StatusUnauthorized,
	4: http.StatusForbidden, 5: http.StatusBadRequest, 6: http.StatusBadRequest, 7: http.StatusBadRequest,
	9: http.StatusServiceUnavailable, 12: http.StatusBadRequest, 13: http.StatusBadRequest, 26: http.StatusTooManyRequests}

// Server is a fake HTTP Event Collector over HTTPS, with one token.
type Server struct {
	*httptest.Server
	// Token is the HEC token it takes.
	Token string
	// Indexes are the indexes the token may write to; none: any.
	Indexes []string

	mu       sync.Mutex
	events   []Event
	requests int
	state    *Reply // answers every request when set: busy, at capacity, token disabled
}

// New starts a collector for the test, with a random token; its certificate is Certificate().
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Token: uuid.NewString()}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Events returns the events the collector took, in the order it took them.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

// Requests returns how many requests reached the endpoint.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Answer makes the collector answer every request with r (ServerBusy, QueueAtCapacity,
// TokenDisabled), taking nothing; nil restores it.
func (s *Server) Answer(r *Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = r
}

func reply(w http.ResponseWriter, r Reply) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(statuses[r.Code])
	_ = json.NewEncoder(w).Encode(r)
}

func at(r Reply, i int) Reply {
	r.Event = &i
	return r
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != Path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.requests++
	state := s.state
	s.mu.Unlock()
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Splunk ")
	switch {
	case auth == "":
		reply(w, TokenRequired)
		return
	case !ok:
		reply(w, InvalidAuth)
		return
	case token != s.Token:
		reply(w, InvalidToken)
		return
	case state != nil:
		reply(w, *state)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reply(w, InvalidDataFormat)
		return
	}
	events, bad, n := s.parse(body)
	switch {
	case bad != nil && bad.Code == NoData.Code:
		reply(w, NoData)
	case bad != nil:
		reply(w, at(*bad, n))
	default:
		s.mu.Lock()
		s.events = append(s.events, events...)
		s.mu.Unlock()
		reply(w, Success)
	}
}

// parse reads a request's events: JSON objects one after the other, or a JSON array of them. A
// request with an invalid event is refused: parse returns why, and the event's number, from 0.
func (s *Server) parse(body []byte) ([]Event, *Reply, int) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, &NoData, 0
	}
	var raws []json.RawMessage
	if body[0] == '[' {
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, &InvalidDataFormat, 0
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(body))
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, &InvalidDataFormat, len(raws)
			}
			raws = append(raws, raw)
		}
	}
	events := make([]Event, 0, len(raws))
	for i, raw := range raws {
		var ev Event
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			return nil, &InvalidDataFormat, i
		}
		switch {
		case len(ev.Event) == 0 || string(ev.Event) == "null":
			return nil, &EventRequired, i
		case string(ev.Event) == `""`:
			return nil, &EventBlank, i
		case ev.Index != "" && len(s.Indexes) > 0 && !slices.Contains(s.Indexes, ev.Index):
			return nil, &IncorrectIndex, i
		}
		events = append(events, ev)
	}
	return events, nil, 0
}
