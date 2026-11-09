// Package servicenowtest is a fake ServiceNow instance, for the tests of Quai's change requests
// (docs/plan/phase-4-backlog.md#services-with-no-account): there is no ServiceNow instance to test
// with. It is an HTTPS server with a certificate of its own (CAFile trusts it) that implements
// only what Quai calls, refuses what the documentation says is refused, and keeps what it received
// for the test to read:
//
//   - OAuth's token endpoint, for the client credentials grant: POST /oauth_token.do, a form with
//     grant_type=client_credentials, client_id and client_secret; it answers {access_token,
//     token_type, expires_in, scope}, or 401 {error, error_description}.
//   - The Change Management API: create a normal change (POST /api/sn_chg_rest/change/normal) or
//     a standard change from a template (POST /api/sn_chg_rest/change/standard/{template sys_id}),
//     the fields as query parameters, no body; read a change (GET /api/sn_chg_rest/change/{sys_id})
//     and update one (PATCH /api/sn_chg_rest/change/{sys_id}, the fields in a JSON body), such as
//     its state to request approval. Each answers {"result": {...}}, each field a {value,
//     display_value} pair, with __meta.ignoredFields on a create or an update.
//   - The Table API on change_request: query (GET /api/now/table/change_request with
//     sysparm_query, sysparm_fields, sysparm_limit; {"result": [...]}, plain values) and update a
//     record (PATCH /api/now/table/change_request/{sys_id}, a JSON body), work notes included.
//   - The Table API on incident: query (GET /api/now/table/incident), read one (GET
//     /api/now/table/incident/{sys_id}) and update one (PATCH, its rfc field: the change request
//     that fixes it). A reference field is {link, value} unless sysparm_exclude_reference_link is
//     true.
//   - The Attachment API: upload a file (POST /api/now/attachment/file with table_name,
//     table_sys_id and file_name, the file as the body, of its Content-Type; 201 and its metadata)
//     and list attachments (GET /api/now/attachment with sysparm_query and sysparm_limit).
//
// Each /api/ call needs Authorization: Bearer with a token the endpoint issued that has not
// expired, or Basic with the service account's user name and password; errors are
// {"error": {"message", "detail"}, "status": "failure"}.
//
// It follows ServiceNow's documentation, read on 2026-11-09:
//   - Change Management API
//     (https://www.servicenow.com/docs/r/api-reference/rest-apis/change-management-api.html):
//     the endpoints above, their status codes (200, 400, 401, 404, 500), the fields of a create as
//     query parameters with no request body, a new normal change's state (-5, "New") and type, the
//     fields a standard change ignores at creation (description, backout plan, test plan,
//     implementation plan, and those not on the template), __meta.ignoredFields, a PATCH's body
//     overriding its query, value and display_value pairs, the approval field ("not requested" by
//     default, "approved"), the roles (itil, sn_change_read, change_manager).
//   - Table API (https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html):
//     sysparm_query, sysparm_fields, sysparm_limit, sysparm_exclude_reference_link, an empty result
//     array when nothing matches, a PATCH's JSON body of name-value pairs, 404 for no record.
//   - State model and transitions
//     (https://www.servicenow.com/docs/bundle/yokohama-it-service-management/page/product/change-management/concept/c_ChangeStateModel.html):
//     the states New (-5), Assess (-4), Authorize (-3), Scheduled (-2), Implement (-1), Review (0),
//     Closed (3) and Canceled (4); a normal change goes through each; a standard change is
//     pre-authorized and skips Assess and Authorize; a rejected change goes back to New; a change
//     is canceled from any state but Review and Closed; Implement is when the work is done, Review
//     when the change requester determines whether it was successful, then Closed.
//   - Update change request states (https://www.servicenow.com/docs/r/Recj91eIo1YnsOabQjgskg/XrCKLFA4Q3O9sjVkX6VY8A):
//     a change is closed with a close code (successful, successful_issues, unsuccessful) and close
//     notes.
//   - Attachment API (https://www.servicenow.com/docs/r/api-reference/rest-apis/c_AttachmentAPI.html):
//     POST /now/attachment/file and its query parameters (table_name, table_sys_id and file_name,
//     required), its required Content-Type, its answer (201; sys_id, file_name, table_name,
//     table_sys_id, size_bytes, content_type, download_link) and errors (400, 403, 404); GET
//     /now/attachment with sysparm_query, sysparm_limit and sysparm_offset; the roles (those that
//     create sys_attachment records and those that read and write the table); the size limit
//     (com.glide.attachment.max_size, 1024 MB by default).
//   - The Table API's incident example: the fields sys_id, number (INC0000046), active ("true"),
//     state ("1"), rfc and caused_by; an invalid query's part ignored by default
//     (glide.invalid_query.returns_no_rows).
//   - Change request related tabs
//     (https://www.servicenow.com/docs/r/4xj1xH858qF8uWRIN94F3g/EVgG2_3lxZl_XApnr2JBIQ): a change
//     request lists its "Incidents Fixed By Change" and "Incidents Caused By Change".
//   - Integer values for default choice lists
//     (https://www.servicenow.com/docs/r/fBdxWC~8p08POQuUubVDig/87hsB7R2QyetNfUQLse2Yg): an
//     incident's states, New (1), In Progress (2), On Hold (3), Resolved (6), Closed (7) and
//     Canceled (8); a closed incident is not active.
//   - Client credentials grant workflow
//     (https://www.servicenow.com/docs/r/platform-security/authentication/client-credentials-grant-workflow.html):
//     POST /oauth_token.do with grant_type, client_id and client_secret, then Authorization: Bearer.
//
// What the pages do not say, the fake assumes, and it is not verified against a real instance:
// the token answer is OAuth 2.0's (RFC 6749, 5.1), with ServiceNow's default lifespan of 1800
// seconds, and its form is application/x-www-form-urlencoded; the approval field's other values
// ("requested" once approval is requested); a standard change's approval is "approved" once it is
// scheduled; the error body's text for 401 and 404; a throttled call answers 429 with Retry-After
// (ServiceNow's rate limit rules); the length of correlation_id (100); that the Change
// Management API's PATCH moves a change to Implement, Review and Closed by name, without waiting
// for its planned start date; that "Incidents Fixed By Change" lists the incidents whose rfc field
// names the change request (ServiceNow's community says so, not the page); that a canceled incident
// is not active; that attachments are answered in the order they were made. The fake's change
// numbers are CHG0030001 and on; its incidents' are the tests'.
package servicenowtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
)

// The documented states of a change request, by value.
const (
	New       = -5
	Assess    = -4
	Authorize = -3
	Scheduled = -2
	Implement = -1
	Review    = 0
	Closed    = 3
	Canceled  = 4
)

// The documented states of an incident, by value.
const (
	IncidentNew        = 1
	IncidentInProgress = 2
	IncidentOnHold     = 3
	IncidentResolved   = 6
	IncidentClosed     = 7
	IncidentCanceled   = 8
)

// closeCodes are the documented close codes of a change request.
var closeCodes = []string{"successful", "successful_issues", "unsuccessful"}

// StateLabels are the states' display values.
var StateLabels = map[int]string{New: "New", Assess: "Assess", Authorize: "Authorize", Scheduled: "Scheduled", Implement: "Implement",
	Review: "Review", Closed: "Closed", Canceled: "Canceled"}

var approvalLabels = map[string]string{"not requested": "Not Yet Requested", "requested": "Requested", "approved": "Approved", "rejected": "Rejected"}

// Fields are the change request fields the fake knows, beyond the ones it sets itself (sys_id,
// number, type, state, approval): any other is ignored, as a field that does not exist.
var Fields = []string{"short_description", "description", "justification", "implementation_plan", "risk_impact_analysis",
	"backout_plan", "test_plan", "correlation_id", "correlation_display", "start_date", "end_date", "assignment_group",
	"category", "cmdb_ci", "work_notes", "comments", "risk", "impact", "close_code", "close_notes"}

// templateIgnored are the fields a standard change ignores at its creation.
var templateIgnored = []string{"description", "backout_plan", "test_plan", "implementation_plan"}

// Change is a change request of the fake.
type Change struct {
	SysID, Number string
	Type          string // normal or standard
	Template      string // a standard change's template's sys_id
	State         int
	Approval      string
	// StateValue and ApprovalValue, when set, are answered instead of State and Approval: values
	// ServiceNow does not document, for the tests of what Quai does with them.
	StateValue, ApprovalValue string
	Fields                    map[string]string
	WorkNotes                 []string
}

// Incident is an incident of the fake.
type Incident struct {
	SysID, Number string
	State         int    // its state's value (IncidentNew…)
	Change        string // rfc: the sys_id of the change request that fixes it, or ""
}

// Active is the incident's active flag: false once it is closed or canceled.
func (i Incident) Active() bool { return i.State != IncidentClosed && i.State != IncidentCanceled }

// Attachment is a file attached to a record of the fake.
type Attachment struct {
	SysID, Table, Record, FileName, ContentType string
	Data                                        []byte
}

// Template is a standard change template, and the fields its changes start with.
type Template struct {
	SysID  string
	Fields map[string]string
}

// Call is a call the instance received.
type Call struct {
	Method, Path string
	Query        url.Values
	Body         string
	Auth         string // bearer, basic, or none
}

// Server is the fake instance, with one OAuth application and one service account.
type Server struct {
	*httptest.Server
	ClientID, ClientSecret string
	Username, Password     string
	// Lifetime is the tokens'.
	Lifetime time.Duration
	// CAFile is a PEM file of the server's certificate, for netx.New.
	CAFile string

	mu        sync.Mutex
	now       func() time.Time
	tokens    map[string]time.Time
	issued    int
	changes   []*Change
	incidents []*Incident
	files     []*Attachment
	templates map[string]*Template
	calls     []Call
	down      bool
	throttled int
	retry     string
	lose      int // creates made whose answer is lost (500)
	loseFiles int // uploads made whose answer is lost (500)
	loseLinks int // incident updates made whose answer is lost (500)
	loseMoves int // state moves made whose answer is lost (500)
	refuse    map[string]int
	drop      string        // the prefix of the paths whose calls fail as when down
	moves     int           // state moves to refuse, as the state model would
	hold      chan struct{} // the next reading of a change request waits until it is closed
	reached   chan struct{} // closed when that reading arrives
}

// Start starts the instance for the test, with credentials of its own.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{ClientID: random(16), ClientSecret: random(20), Username: "quai.integration", Password: random(20),
		Lifetime: 1800 * time.Second, now: time.Now, tokens: map[string]time.Time{}, templates: map[string]*Template{}, refuse: map[string]int{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	ca := filepath.Join(t.TempDir(), "servicenow-ca.pem")
	if err := os.WriteFile(ca, s.PEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	s.CAFile = ca
	return s
}

// PEM returns the server's certificate, to put in one CA file with others'.
func (s *Server) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// OAuth returns the configuration of Quai with the OAuth application, its secret at secretRef.
func (s *Server) OAuth(secretRef string) *config.ServiceNow {
	return &config.ServiceNow{InstanceURL: s.URL, OAuth: &config.ServiceNowOAuth{ClientID: s.ClientID, ClientSecret: config.SecretRef{Ref: secretRef}},
		PollInterval: config.DefaultServiceNowPoll}
}

// Basic returns the configuration of Quai with the service account, its password at secretRef.
func (s *Server) Basic(secretRef string) *config.ServiceNow {
	return &config.ServiceNow{InstanceURL: s.URL, Basic: &config.ServiceNowBasic{Username: s.Username, Password: config.SecretRef{Ref: secretRef}},
		PollInterval: config.DefaultServiceNowPoll}
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// UseClock sets the clock tokens expire by.
func (s *Server) UseClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// AddTemplate adds a standard change template with the fields its changes start with, and
// returns its sys_id.
func (s *Server) AddTemplate(fields map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Template{SysID: random(16), Fields: maps.Clone(fields)}
	s.templates[t.SysID] = t
	return t.SysID
}

// Down makes the instance close every connection without an answer (true), as one that is down
// or cannot be reached, or serve again (false). A 503 is Throttle's 429's twin, retried the same.
func (s *Server) Down(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// Throttle makes the next n calls answer 429 Too Many Requests with Retry-After (if not empty).
func (s *Server) Throttle(n int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttled, s.retry = n, retryAfter
}

// LoseCreates makes the next n creates make the change request and answer 500, as when the
// answer is lost (a timeout, a crash): what a retried creation must find again.
func (s *Server) LoseCreates(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lose = n
}

// LoseAttachments makes the next n uploads attach the file and answer 500, as when the answer is
// lost: what a retried upload must find again.
func (s *Server) LoseAttachments(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loseFiles = n
}

// LoseLinks makes the next n updates of an incident take and answer 500.
func (s *Server) LoseLinks(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loseLinks = n
}

// AddIncident adds an incident with a number (two may have the same) in a state, and returns its
// sys_id.
func (s *Server) AddIncident(number string, state int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := &Incident{SysID: random(16), Number: number, State: state}
	s.incidents = append(s.incidents, i)
	return i.SysID
}

// Incident returns a copy of the incident with sys_id.
func (s *Server) Incident(sysID string) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.incidents {
		if i.SysID == sysID {
			return *i, true
		}
	}
	return Incident{}, false
}

// SetIncident changes an incident as a person in ServiceNow would: its state, its change request.
func (s *Server) SetIncident(sysID string, state int, change string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.incidents {
		if i.SysID == sysID {
			i.State, i.Change = state, change
			return
		}
	}
	panic("servicenowtest: no incident " + sysID)
}

// Attachments returns copies of the files attached to the record with sys_id, in the order they
// were attached.
func (s *Server) Attachments(record string) []Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Attachment
	for _, a := range s.files {
		if a.Record == record {
			c := *a
			c.Data = slices.Clone(a.Data)
			out = append(out, c)
		}
	}
	return out
}

// Drop makes every call whose path starts with prefix fail as Down makes every call fail, the
// connection closed unanswered, until Drop(""). A count would not do: Go's client sends an
// idempotent call again on its own when a connection it reused closes.
func (s *Server) Drop(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = prefix
}

// Refuse makes the next n calls whose path starts with prefix answer 400.
func (s *Server) Refuse(prefix string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[prefix] = n
}

// LoseMoves makes the next n state moves of the Change Management API happen, but answer 500, as
// a move whose answer was lost.
func (s *Server) LoseMoves(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loseMoves = n
}

// RefuseMoves makes the next n state moves fail as one the state model does not allow.
func (s *Server) RefuseMoves(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moves = n
}

// HoldRead makes the next reading of a change request (the Change Management API's GET) wait
// until release is called, and returns a channel closed when it arrives: what the change request
// says is read after the release, as if the answer took that long.
func (s *Server) HoldRead() (arrived <-chan struct{}, release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.reached = make(chan struct{}), make(chan struct{})
	hold := s.hold
	var once sync.Once
	return s.reached, func() { once.Do(func() { close(hold) }) }
}

// ExpireTokens makes every token issued so far expired.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.tokens {
		s.tokens[t] = time.Time{}
	}
}

// Tokens returns how many tokens the endpoint issued.
func (s *Server) Tokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued
}

// Calls returns the calls the instance received, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Changes returns copies of the change requests, in the order they were made.
func (s *Server) Changes() []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Change, len(s.changes))
	for i, c := range s.changes {
		out[i] = *c
		out[i].Fields, out[i].WorkNotes = maps.Clone(c.Fields), slices.Clone(c.WorkNotes)
	}
	return out
}

// Change returns a copy of the change request with sys_id.
func (s *Server) Change(sysID string) (Change, bool) {
	for _, c := range s.Changes() {
		if c.SysID == sysID {
			return c, true
		}
	}
	return Change{}, false
}

// Set changes a change request as a person in ServiceNow would: its state, its approval.
func (s *Server) Set(sysID string, state int, approval string) {
	s.update(sysID, func(c *Change) { c.State, c.Approval, c.StateValue, c.ApprovalValue = state, approval, "", "" })
}

// SetRaw makes a change request answer a state and an approval ServiceNow does not document.
func (s *Server) SetRaw(sysID, state, approval string) {
	s.update(sysID, func(c *Change) { c.StateValue, c.ApprovalValue = state, approval })
}

// Approve approves a change request, as its approvers would: scheduled and approved.
func (s *Server) Approve(sysID string) { s.Set(sysID, Scheduled, "approved") }

// Reject rejects a change request: back to New, rejected.
func (s *Server) Reject(sysID string) { s.Set(sysID, New, "rejected") }

// Cancel cancels a change request, its approval as it was.
func (s *Server) Cancel(sysID string) {
	s.update(sysID, func(c *Change) { c.State, c.StateValue = Canceled, "" })
}

// SetField sets a field of a change request, as a person in ServiceNow could.
func (s *Server) SetField(sysID, field, value string) {
	s.update(sysID, func(c *Change) { c.Fields[field] = value })
}

func (s *Server) update(sysID string, fn func(*Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.changes {
		if c.SysID == sysID {
			fn(c)
			return
		}
	}
	panic("servicenowtest: no change request " + sysID)
}

func failure(w http.ResponseWriter, status int, message, detail string) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "detail": detail}, "status": "failure"})
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	const cm, table, incidents, files = "/api/sn_chg_rest/change/", "/api/now/table/change_request", "/api/now/table/incident", "/api/now/attachment"
	limit := int64(1 << 20)
	if r.URL.Path == files+"/file" {
		limit = 64 << 20
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, limit))
	if rest, ok := strings.CutPrefix(r.URL.Path, cm); ok && r.Method == http.MethodGet && !strings.Contains(rest, "/") {
		s.mu.Lock()
		hold, reached := s.hold, s.reached
		s.hold, s.reached = nil, nil
		s.mu.Unlock()
		if hold != nil {
			close(reached)
			<-hold
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	call := Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: string(body), Auth: "none"}
	auth := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		call.Auth = "bearer"
	case strings.HasPrefix(auth, "Basic "):
		call.Auth = "basic"
	}
	switch r.URL.Path {
	case "/oauth_token.do":
		call.Body = "[form]" // it holds the client secret
	case files + "/file":
		call.Body = fmt.Sprintf("[%d bytes]", len(body))
	}
	s.calls = append(s.calls, call)
	if s.down || s.drop != "" && strings.HasPrefix(r.URL.Path, s.drop) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if s.throttled > 0 {
		s.throttled--
		if s.retry != "" {
			w.Header().Set("Retry-After", s.retry)
		}
		failure(w, http.StatusTooManyRequests, "Too many requests", "The rate limit was exceeded")
		return
	}
	for prefix, n := range s.refuse {
		if n > 0 && strings.HasPrefix(r.URL.Path, prefix) {
			s.refuse[prefix] = n - 1
			failure(w, http.StatusBadRequest, "Invalid request", "The fake refuses this call")
			return
		}
	}
	if r.URL.Path == "/oauth_token.do" {
		s.token(w, r, body)
		return
	}
	if !s.authorized(auth) {
		failure(w, http.StatusUnauthorized, "User Not Authenticated", "Required to provide Auth information")
		return
	}
	switch rest, isCM := strings.CutPrefix(r.URL.Path, cm); {
	case r.Method == http.MethodPost && r.URL.Path == cm+"normal":
		s.create(w, r, "normal", nil)
	case r.Method == http.MethodPost && isCM && strings.HasPrefix(rest, "standard/") && strings.Count(rest, "/") == 1:
		t, ok := s.templates[strings.TrimPrefix(rest, "standard/")]
		if !ok {
			failure(w, http.StatusNotFound, "No Record found", "Standard change template doesn't exist or is not active")
			return
		}
		s.create(w, r, "standard", t)
	case isCM && !strings.Contains(rest, "/") && (r.Method == http.MethodGet || r.Method == http.MethodPatch):
		c := s.find(rest)
		if c == nil {
			failure(w, http.StatusNotFound, "No Record found", "Record doesn't exist or ACL restricts the record retrieval")
			return
		}
		var ignored []string
		if r.Method == http.MethodPatch {
			var in map[string]string
			if err := json.Unmarshal(body, &in); err != nil {
				failure(w, http.StatusBadRequest, "Invalid request", "The body is not a JSON object of strings")
				return
			}
			for k, v := range r.URL.Query() {
				if _, ok := in[k]; !ok && len(v) > 0 {
					in[k] = v[0]
				}
			}
			var err error
			if ignored, err = s.patch(c, in); err != nil {
				failure(w, http.StatusBadRequest, "Invalid state transition", err.Error())
				return
			}
			if _, moved := in["state"]; moved && s.loseMoves > 0 {
				s.loseMoves--
				failure(w, http.StatusInternalServerError, "Transaction cancelled: maximum execution time exceeded", "")
				return
			}
		}
		answer(w, map[string]any{"result": cmRecord(c, ignored)})
	case r.Method == http.MethodGet && r.URL.Path == table:
		var rows []map[string]string
		for _, c := range s.changes {
			rows = append(rows, tableRecord(c, ""))
		}
		query(w, r, rows)
	case r.Method == http.MethodGet && r.URL.Path == incidents:
		var rows []map[string]string
		for _, i := range s.incidents {
			rows = append(rows, incidentRecord(i))
		}
		query(w, r, rows)
	case strings.HasPrefix(r.URL.Path, incidents+"/") && (r.Method == http.MethodGet || r.Method == http.MethodPatch):
		s.incident(w, r, strings.TrimPrefix(r.URL.Path, incidents+"/"), body)
	case r.Method == http.MethodPost && r.URL.Path == files+"/file":
		s.attach(w, r, body)
	case r.Method == http.MethodGet && r.URL.Path == files:
		var rows []map[string]string
		for _, a := range s.files {
			rows = append(rows, s.fileRecord(a))
		}
		query(w, r, rows)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, table+"/"):
		c := s.find(strings.TrimPrefix(r.URL.Path, table+"/"))
		if c == nil {
			failure(w, http.StatusNotFound, "No Record found", "Record doesn't exist or ACL restricts the record retrieval")
			return
		}
		var in map[string]string
		if err := json.Unmarshal(body, &in); err != nil {
			failure(w, http.StatusBadRequest, "Invalid request", "The body is not a JSON object of strings")
			return
		}
		for k, v := range in {
			switch {
			case k == "work_notes":
				if v != "" {
					c.WorkNotes = append(c.WorkNotes, v)
				}
			case slices.Contains(Fields, k):
				c.Fields[k] = v
			}
		}
		answer(w, map[string]any{"result": tableRecord(c, r.URL.Query().Get("sysparm_fields"))})
	default:
		failure(w, http.StatusBadRequest, "Requested URI does not represent any resource", r.URL.Path)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request, body []byte) {
	form, err := url.ParseQuery(string(body))
	if r.Method != http.MethodPost || err != nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		w.WriteHeader(http.StatusBadRequest)
		answer(w, map[string]string{"error": "invalid_request", "error_description": "invalid_request"})
		return
	}
	id, secret := form.Get("client_id"), form.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		id, secret = u, p
	}
	if form.Get("grant_type") != "client_credentials" || id != s.ClientID || secret != s.ClientSecret {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied", "error_description": "access_denied"})
		return
	}
	tok := random(32)
	s.tokens[tok] = s.now().Add(s.Lifetime)
	s.issued++
	answer(w, map[string]any{"access_token": tok, "scope": "useraccount", "token_type": "Bearer", "expires_in": int(s.Lifetime.Seconds())})
}

func (s *Server) authorized(auth string) bool {
	if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
		exp, known := s.tokens[tok]
		return known && s.now().Before(exp)
	}
	r := http.Request{Header: http.Header{"Authorization": {auth}}}
	u, p, ok := r.BasicAuth()
	return ok && u == s.Username && p == s.Password
}

func (s *Server) find(sysID string) *Change {
	for _, c := range s.changes {
		if c.SysID == sysID {
			return c
		}
	}
	return nil
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, kind string, t *Template) {
	c := &Change{SysID: random(16), Number: fmt.Sprintf("CHG%07d", 30001+len(s.changes)), Type: kind, State: New, Approval: "not requested",
		Fields: map[string]string{}}
	var ignored []string
	if t != nil {
		c.Template = t.SysID
		maps.Copy(c.Fields, t.Fields)
	}
	q := r.URL.Query()
	for _, k := range slices.Sorted(maps.Keys(q)) {
		v := q.Get(k)
		switch {
		case !slices.Contains(Fields, k) || k == "work_notes",
			t != nil && slices.Contains(templateIgnored, k):
			ignored = append(ignored, k)
		default:
			c.Fields[k] = v
		}
	}
	s.changes = append(s.changes, c)
	if s.lose > 0 {
		s.lose--
		failure(w, http.StatusInternalServerError, "Transaction cancelled: maximum execution time exceeded", "")
		return
	}
	answer(w, map[string]any{"result": cmRecord(c, ignored)})
}

// patch applies an update of the Change Management API: a state by its name or value, through the
// transitions the state model allows; the other fields as the Table API does.
func (s *Server) patch(c *Change, in map[string]string) ([]string, error) {
	var ignored []string
	for _, k := range slices.Sorted(maps.Keys(in)) {
		v := in[k]
		switch {
		case k == "state":
			to, ok := stateOf(v)
			if !ok {
				return nil, fmt.Errorf("unknown state %q", v)
			}
			if s.moves > 0 {
				s.moves--
				return nil, fmt.Errorf("%s cannot move from %s to %s", c.Number, StateLabels[c.State], StateLabels[to])
			}
			switch {
			case c.State == New && to == Assess && c.Type == "normal":
				c.State, c.Approval = Assess, "requested"
			case c.State == New && to == Scheduled && c.Type == "standard":
				c.State, c.Approval = Scheduled, "approved"
			case c.State == Scheduled && to == Implement && c.Approval == "approved":
				c.State = Implement
			case c.State == Implement && to == Review:
				c.State = Review
			case c.State == Review && to == Closed:
				if !slices.Contains(closeCodes, in["close_code"]) || strings.TrimSpace(in["close_notes"]) == "" {
					return nil, fmt.Errorf("%s cannot be closed without a close code and close notes", c.Number)
				}
				c.State = Closed
			case to == Canceled && c.State != Review && c.State != Closed:
				c.State = Canceled
			default:
				return nil, fmt.Errorf("%s cannot move from %s to %s", c.Number, StateLabels[c.State], StateLabels[to])
			}
		case k == "work_notes":
			c.WorkNotes = append(c.WorkNotes, v)
		case slices.Contains(Fields, k):
			c.Fields[k] = v
		default:
			ignored = append(ignored, k)
		}
	}
	return ignored, nil
}

func stateOf(v string) (int, bool) {
	if n, err := strconv.Atoi(v); err == nil {
		_, ok := StateLabels[n]
		return n, ok
	}
	for n, l := range StateLabels {
		if strings.EqualFold(l, v) {
			return n, true
		}
	}
	return 0, false
}

func (c *Change) values() map[string][2]string {
	state, approval := strconv.Itoa(c.State), c.Approval
	stateLabel, approvalLabel := StateLabels[c.State], approvalLabels[c.Approval]
	if c.StateValue != "" {
		state, stateLabel = c.StateValue, c.StateValue
	}
	if c.ApprovalValue != "" {
		approval, approvalLabel = c.ApprovalValue, c.ApprovalValue
	}
	v := map[string][2]string{"sys_id": {c.SysID, c.SysID}, "number": {c.Number, c.Number}, "type": {c.Type, strings.ToUpper(c.Type[:1]) + c.Type[1:]},
		"state": {state, stateLabel}, "approval": {approval, approvalLabel}, "work_notes": {"", ""}}
	for _, f := range Fields {
		if f != "work_notes" {
			v[f] = [2]string{c.Fields[f], c.Fields[f]}
		}
	}
	return v
}

// cmRecord is a change request as the Change Management API answers it: each field a value and
// display_value pair; state's value a string, as in a create's answer.
func cmRecord(c *Change, ignored []string) map[string]any {
	out := map[string]any{}
	for k, v := range c.values() {
		out[k] = map[string]string{"value": v[0], "display_value": v[1]}
	}
	if ignored != nil {
		out["__meta"] = map[string]any{"ignoredFields": ignored}
	}
	return out
}

// tableRecord is a change request as the Table API answers it: plain values, only fields when set.
func tableRecord(c *Change, fields string) map[string]string {
	out := map[string]string{}
	for k, v := range c.values() {
		out[k] = v[0]
	}
	return only(out, fields)
}

// query answers a query of rows (each a record's plain values): field=value conditions joined by
// ^, the fields asked for (sysparm_fields), at most sysparm_limit.
func query(w http.ResponseWriter, r *http.Request, rows []map[string]string) {
	q := r.URL.Query()
	limit := 10000
	if l := q.Get("sysparm_limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			failure(w, http.StatusBadRequest, "Invalid sysparm_limit", l)
			return
		}
		limit = n
	}
	var conds [][2]string
	if sq := q.Get("sysparm_query"); sq != "" {
		for _, c := range strings.Split(sq, "^") {
			k, v, ok := strings.Cut(c, "=")
			if !ok {
				failure(w, http.StatusBadRequest, "The fake knows only field=value conditions", c)
				return
			}
			conds = append(conds, [2]string{k, v})
		}
	}
	result := []map[string]string{}
	for _, row := range rows {
		if !slices.ContainsFunc(conds, func(cond [2]string) bool { return row[cond[0]] != cond[1] }) && len(result) < limit {
			result = append(result, only(row, q.Get("sysparm_fields")))
		}
	}
	answer(w, map[string]any{"result": result})
}

// only is a record's fields that were asked for (all without sysparm_fields).
func only(row map[string]string, fields string) map[string]string {
	if fields == "" {
		return row
	}
	out := map[string]string{}
	for _, f := range strings.Split(fields, ",") {
		if v, ok := row[f]; ok {
			out[f] = v
		}
	}
	return out
}

// incidentRecord is an incident's plain values, for a query; its rfc a sys_id (the reference's
// link is in incidentAnswer).
func incidentRecord(i *Incident) map[string]string {
	return map[string]string{"sys_id": i.SysID, "number": i.Number, "state": strconv.Itoa(i.State), "active": strconv.FormatBool(i.Active()),
		"rfc": i.Change, "caused_by": "", "close_code": ""}
}

// incidentAnswer is an incident as the Table API answers it: its rfc a {link, value} reference
// unless sysparm_exclude_reference_link is true.
func (s *Server) incidentAnswer(i *Incident, q url.Values) map[string]any {
	out := map[string]any{}
	for k, v := range only(incidentRecord(i), q.Get("sysparm_fields")) {
		out[k] = v
	}
	if _, ok := out["rfc"]; ok && i.Change != "" && q.Get("sysparm_exclude_reference_link") != "true" {
		out["rfc"] = map[string]string{"link": s.URL + "/api/now/table/change_request/" + i.Change, "value": i.Change}
	}
	return out
}

// incident reads or updates an incident: its rfc (a change request's sys_id) and work notes.
func (s *Server) incident(w http.ResponseWriter, r *http.Request, sysID string, body []byte) {
	var inc *Incident
	for _, i := range s.incidents {
		if i.SysID == sysID {
			inc = i
		}
	}
	if inc == nil {
		failure(w, http.StatusNotFound, "No Record found", "Record doesn't exist or ACL restricts the record retrieval")
		return
	}
	if r.Method == http.MethodPatch {
		var in map[string]string
		if err := json.Unmarshal(body, &in); err != nil {
			failure(w, http.StatusBadRequest, "Invalid request", "The body is not a JSON object of strings")
			return
		}
		if rfc, ok := in["rfc"]; ok {
			if rfc != "" && s.find(rfc) == nil {
				failure(w, http.StatusBadRequest, "Invalid reference", "rfc names no change request")
				return
			}
			inc.Change = rfc
		}
		if s.loseLinks > 0 {
			s.loseLinks--
			failure(w, http.StatusInternalServerError, "Transaction cancelled: maximum execution time exceeded", "")
			return
		}
	}
	answer(w, map[string]any{"result": s.incidentAnswer(inc, r.URL.Query())})
}

// fileRecord is an attachment's metadata, as the Attachment API answers it.
func (s *Server) fileRecord(a *Attachment) map[string]string {
	return map[string]string{"sys_id": a.SysID, "file_name": a.FileName, "table_name": a.Table, "table_sys_id": a.Record,
		"size_bytes": strconv.Itoa(len(a.Data)), "content_type": a.ContentType, "download_link": s.URL + "/api/now/attachment/" + a.SysID + "/file"}
}

// attach uploads a file to a change request (the only table the fake has).
func (s *Server) attach(w http.ResponseWriter, r *http.Request, body []byte) {
	q := r.URL.Query()
	table, record, name, ct := q.Get("table_name"), q.Get("table_sys_id"), q.Get("file_name"), r.Header.Get("Content-Type")
	switch {
	case table == "" || record == "" || name == "":
		failure(w, http.StatusBadRequest, "Missing parameter", "table_name, table_sys_id and file_name are required")
		return
	case ct == "":
		failure(w, http.StatusBadRequest, "Missing Content-Type", "Content-Type is required")
		return
	case table != "change_request" || s.find(record) == nil:
		failure(w, http.StatusNotFound, "No Record found", "Record doesn't exist or ACL restricts the record retrieval")
		return
	}
	a := &Attachment{SysID: random(16), Table: table, Record: record, FileName: name, ContentType: ct, Data: slices.Clone(body)}
	s.files = append(s.files, a)
	if s.loseFiles > 0 {
		s.loseFiles--
		failure(w, http.StatusInternalServerError, "Transaction cancelled: maximum execution time exceeded", "")
		return
	}
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": s.fileRecord(a)})
}
