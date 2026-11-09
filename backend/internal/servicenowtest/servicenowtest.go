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
//     is canceled from any state but Review and Closed.
//   - Client credentials grant workflow
//     (https://www.servicenow.com/docs/r/platform-security/authentication/client-credentials-grant-workflow.html):
//     POST /oauth_token.do with grant_type, client_id and client_secret, then Authorization: Bearer.
//
// What the pages do not say, the fake assumes, and it is not verified against a real instance:
// the token answer is OAuth 2.0's (RFC 6749, 5.1), with ServiceNow's default lifespan of 1800
// seconds, and its form is application/x-www-form-urlencoded; the approval field's other values
// ("requested" once approval is requested); a standard change's approval is "approved" once it is
// scheduled; the error body's text for 401 and 404; a throttled call answers 429 with Retry-After
// (ServiceNow's rate limit rules); the length of correlation_id (100). The fake's change numbers
// are CHG0030001 and on.
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

// StateLabels are the states' display values.
var StateLabels = map[int]string{New: "New", Assess: "Assess", Authorize: "Authorize", Scheduled: "Scheduled", Implement: "Implement",
	Review: "Review", Closed: "Closed", Canceled: "Canceled"}

var approvalLabels = map[string]string{"not requested": "Not Yet Requested", "requested": "Requested", "approved": "Approved", "rejected": "Rejected"}

// Fields are the change request fields the fake knows, beyond the ones it sets itself (sys_id,
// number, type, state, approval): any other is ignored, as a field that does not exist.
var Fields = []string{"short_description", "description", "justification", "implementation_plan", "risk_impact_analysis",
	"backout_plan", "test_plan", "correlation_id", "correlation_display", "start_date", "end_date", "assignment_group",
	"category", "cmdb_ci", "work_notes", "comments", "risk", "impact"}

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
	templates map[string]*Template
	calls     []Call
	down      bool
	throttled int
	retry     string
	lose      int // creates made whose answer is lost (500)
	refuse    map[string]int
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

// Refuse makes the next n calls whose path starts with prefix answer 400.
func (s *Server) Refuse(prefix string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[prefix] = n
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
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	const cm, table = "/api/sn_chg_rest/change/", "/api/now/table/change_request"
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
	if r.URL.Path == "/oauth_token.do" {
		call.Body = "[form]" // it holds the client secret
	}
	s.calls = append(s.calls, call)
	if s.down {
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
		}
		answer(w, map[string]any{"result": cmRecord(c, ignored)})
	case r.Method == http.MethodGet && r.URL.Path == table:
		s.query(w, r)
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
	if fields == "" {
		return out
	}
	only := map[string]string{}
	for _, f := range strings.Split(fields, ",") {
		if v, ok := out[f]; ok {
			only[f] = v
		}
	}
	return only
}

// query answers a Table API query of field=value conditions joined by ^.
func (s *Server) query(w http.ResponseWriter, r *http.Request) {
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
	for _, c := range s.changes {
		vals := c.values()
		if !slices.ContainsFunc(conds, func(cond [2]string) bool { return vals[cond[0]][0] != cond[1] }) && len(result) < limit {
			result = append(result, tableRecord(c, q.Get("sysparm_fields")))
		}
	}
	answer(w, map[string]any{"result": result})
}
