// Package servicenow is Quai's client of a ServiceNow instance's change management
// (docs/design/05-delivery-and-approvals.md#itsm): it creates change requests, normal or standard
// from a template, with the Change Management API, finds the one it created for a correlation ID
// with the Table API, reads a change request's state and approval by its sys_id, writes fields and
// work notes, attaches files with the Attachment API and closes it; and it finds an incident by its
// number and links it to a change request, with the Table API.
//
// Every call goes to the configured instance only, through netx (its CAs, its proxy), with TLS
// verified, a timeout, an answer of at most maxAnswer bytes, and no redirect followed; nothing an
// answer holds is called. A throttled call (429, 503) is tried again within maxWait, honouring
// Retry-After; callers try the rest again later. Credentials (the client secret, the password,
// the token) are never in an error or a log.
package servicenow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// Client calls one ServiceNow instance as Quai's service account.
type Client struct {
	cfg     config.ServiceNow
	secrets *secrets.Store
	http    *http.Client
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	mu     sync.Mutex
	token  string
	expiry time.Time
}

const (
	// tokenMargin is how long before a token expires Quai gets another.
	tokenMargin = 2 * time.Minute
	// maxWait is the longest a call waits for a throttled instance; longer, it fails and is
	// tried again later.
	maxWait = 10 * time.Second
	// maxRetries is how often a call is tried again when throttled.
	maxRetries = 2
	// maxAnswer bounds an answer's body.
	maxAnswer = 1 << 20
)

// The documented limits of the fields Quai writes, in characters: task's short_description
// (160) and description (4000) by the dictionary's defaults; the other text fields Quai writes
// the same as description; correlation_id and correlation_display 100.
const (
	MaxShort       = 160
	MaxText        = 4000
	MaxCorrelation = 100
)

// New returns a client of cfg's instance. Outgoing connections trust what network trusts.
func New(cfg config.ServiceNow, network *netx.Network, sec *secrets.Store) (*Client, error) {
	if network == nil {
		var err error
		if network, err = netx.New(""); err != nil {
			return nil, err
		}
	}
	c := &Client{cfg: cfg, secrets: sec, now: time.Now, sleep: sleep}
	c.http = &http.Client{Transport: network.Transport(), Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// UseClock sets the clock tokens expire by, for tests.
func (c *Client) UseClock(now func() time.Time) { c.now = now }

// Instance is the instance's root URL.
func (c *Client) Instance() string { return c.cfg.InstanceURL }

// Link is the address of a change request's form in the instance, from the configured root and a
// sys_id Quai checked: never a link an answer gave.
func (c *Client) Link(sysID string) string {
	if !SysIDRe.MatchString(sysID) {
		return ""
	}
	return c.cfg.InstanceURL + "/change_request.do?sys_id=" + sysID
}

// SysIDRe is a sys_id: 32 lowercase hexadecimal digits.
var SysIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// numberRe is a change request's number, such as CHG0030001.
var numberRe = regexp.MustCompile(`^[A-Z]{2,10}[0-9]{1,16}$`)

// Change is a change request Quai creates.
type Change struct {
	// Template is a standard change's template's sys_id; "" for a normal change.
	Template string
	// Create are the fields set when it is created, Update those set right after (the long
	// ones, in a body): field name to value, already neutral (Text).
	Create, Update map[string]string
}

// Record is a change request as ServiceNow reports it.
type Record struct {
	SysID, Number, Type string
	// State and Approval are the fields' values: State a number (-5 to 4), Approval a word.
	State, Approval string
	// StateLabel is the state's display value.
	StateLabel    string
	CorrelationID string
	// CloseCode is close_code's value: successful, successful_issues or unsuccessful once closed.
	CloseCode string
	// Ignored are the fields ServiceNow said it did not set (__meta.ignoredFields).
	Ignored []string
}

// The outcomes of a change request for its request.
const (
	Pending   = "pending"
	Approved  = "approved"
	Rejected  = "rejected"
	Cancelled = "cancelled"
	Closed    = "closed"
)

// The documented states and approvals (servicenowtest names the pages).
var (
	states    = map[string]string{"-5": "New", "-4": "Assess", "-3": "Authorize", "-2": "Scheduled", "-1": "Implement", "0": "Review", "3": "Closed", "4": "Canceled"}
	approvals = []string{"not requested", "requested", "approved", "rejected"}
)

// Outcome is what a change request says of its request, and whether ServiceNow's values are the
// documented ones. Only an explicit approval while the change is scheduled or being implemented
// approves; a rejection rejects; a canceled or closed change ends the request; anything else, an
// unknown value included, is pending: Quai never approves by default.
func (r Record) Outcome() (string, bool) {
	_, knownState := states[r.State]
	knownApproval := slices.Contains(approvals, r.Approval)
	switch {
	case r.Approval == "rejected":
		return Rejected, true
	case r.State == "4":
		return Cancelled, true
	case r.State == "3":
		return Closed, true
	case !knownState || !knownApproval:
		return Pending, false
	case r.Approval == "approved" && (r.State == "-2" || r.State == "-1"):
		return Approved, true
	}
	return Pending, true
}

// Label is the state's name: its display value when ServiceNow gave one, else the documented one.
func (r Record) Label() string {
	if r.StateLabel != "" {
		return r.StateLabel
	}
	return states[r.State]
}

// StatusError is an answer of the instance that is not a success.
type StatusError struct {
	Status  int
	Message string // the error's message, cut and neutral
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("ServiceNow answered HTTP %d", e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// UnreachableError is a call that did not reach the instance, or that it did not take: a network
// error, a timeout, a 5xx answer, or throttling past maxWait.
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string { return "ServiceNow is unreachable: " + e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// Unreachable reports whether err says the instance is down or busy, rather than refusing.
func Unreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}

// ErrAmbiguous is two change requests with the correlation ID of one Quai created.
var ErrAmbiguous = errors.New("ServiceNow holds more than one change request with this correlation ID")

// Find returns the change request with the correlation ID, nil when there is none: the one a
// create made although its answer was lost. Only an exact match counts.
func (c *Client) Find(ctx context.Context, correlationID string) (*Record, error) {
	if correlationID == "" || strings.ContainsAny(correlationID, "^=") {
		return nil, fmt.Errorf("correlation ID %q cannot be queried", correlationID)
	}
	q := url.Values{"sysparm_query": {"correlation_id=" + correlationID}, "sysparm_limit": {"2"},
		"sysparm_fields": {"sys_id,number,type,state,approval,correlation_id"}, "sysparm_exclude_reference_link": {"true"}}
	var out struct {
		Result []map[string]json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/now/table/change_request", q, nil, &out); err != nil {
		return nil, err
	}
	var found []Record
	for _, raw := range out.Result {
		r, err := record(raw)
		if err != nil {
			return nil, err
		}
		if r.CorrelationID == correlationID {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	}
	return nil, ErrAmbiguous
}

// Create creates a change request: a standard change from its template, or a normal change. The
// fields go in the query, as the API documents; its answer must carry a sys_id.
func (c *Client) Create(ctx context.Context, ch Change) (Record, error) {
	path := "/api/sn_chg_rest/change/normal"
	if ch.Template != "" {
		if !SysIDRe.MatchString(ch.Template) {
			return Record{}, fmt.Errorf("template %q is not a sys_id", ch.Template)
		}
		path = "/api/sn_chg_rest/change/standard/" + ch.Template
	}
	q := url.Values{}
	for k, v := range ch.Create {
		q.Set(k, v)
	}
	return c.change(ctx, http.MethodPost, path, q, nil)
}

// Get reads a change request by its sys_id.
func (c *Client) Get(ctx context.Context, sysID string) (Record, error) {
	if !SysIDRe.MatchString(sysID) {
		return Record{}, fmt.Errorf("%q is not a sys_id", sysID)
	}
	r, err := c.change(ctx, http.MethodGet, "/api/sn_chg_rest/change/"+sysID, nil, nil)
	if err == nil && r.SysID != sysID {
		return Record{}, fmt.Errorf("ServiceNow answered change request %s for %s", r.SysID, sysID)
	}
	return r, err
}

// Move asks ServiceNow to move a change request to a state (assess, scheduled), which its state
// model allows or refuses.
func (c *Client) Move(ctx context.Context, sysID, state string) (Record, error) {
	if !SysIDRe.MatchString(sysID) {
		return Record{}, fmt.Errorf("%q is not a sys_id", sysID)
	}
	return c.change(ctx, http.MethodPatch, "/api/sn_chg_rest/change/"+sysID, nil, map[string]string{"state": state})
}

// Update sets fields of a change request, with the Table API.
func (c *Client) Update(ctx context.Context, sysID string, fields map[string]string) error {
	if !SysIDRe.MatchString(sysID) {
		return fmt.Errorf("%q is not a sys_id", sysID)
	}
	q := url.Values{"sysparm_fields": {"sys_id"}, "sysparm_exclude_reference_link": {"true"}}
	return c.call(ctx, http.MethodPatch, "/api/now/table/change_request/"+sysID, q, fields, nil)
}

// WorkNote adds a work note to a change request; text is made neutral and cut to MaxText.
func (c *Client) WorkNote(ctx context.Context, sysID, text string) error {
	return c.Update(ctx, sysID, map[string]string{"work_notes": Text(text, MaxText)})
}

// The close codes of a change request (the "Update change request states" page).
const (
	CloseSuccessful   = "successful"
	CloseUnsuccessful = "unsuccessful"
)

// Close closes a change request in Review, with its close code and close notes (made neutral and
// cut to MaxText), which ServiceNow requires to close one.
func (c *Client) Close(ctx context.Context, sysID, code, notes string) (Record, error) {
	if !SysIDRe.MatchString(sysID) {
		return Record{}, fmt.Errorf("%q is not a sys_id", sysID)
	}
	if code != CloseSuccessful && code != CloseUnsuccessful {
		return Record{}, fmt.Errorf("close code %q is not one Quai sets", code)
	}
	return c.change(ctx, http.MethodPatch, "/api/sn_chg_rest/change/"+sysID, nil,
		map[string]string{"state": "closed", "close_code": code, "close_notes": Text(notes, MaxText)})
}

// MaxAttachment bounds a file Quai attaches, in bytes.
const MaxAttachment = 10 << 20

// ErrTooLarge is a file over MaxAttachment: it is not sent.
var ErrTooLarge = fmt.Errorf("the file is over %d MiB", MaxAttachment>>20)

// fileNameRe is the name of a file Quai attaches: its own, made of identifiers, never a person's
// text.
var fileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// Attachment is a file attached to a change request, as the Attachment API reports it.
type Attachment struct {
	SysID, FileName string
	// Size is size_bytes; -1 when the answer does not say.
	Size int64
}

// Attach attaches a file to a change request with the Attachment API (POST /api/now/attachment/file,
// the file as the body, of its content type). The answer must name an attachment with a sys_id,
// on that change request, with that file name.
func (c *Client) Attach(ctx context.Context, changeSysID, name, contentType string, data []byte) (Attachment, error) {
	if !SysIDRe.MatchString(changeSysID) {
		return Attachment{}, fmt.Errorf("%q is not a sys_id", changeSysID)
	}
	if !fileNameRe.MatchString(name) {
		return Attachment{}, fmt.Errorf("%q is not a file name Quai attaches", name)
	}
	if len(data) > MaxAttachment {
		return Attachment{}, ErrTooLarge
	}
	q := url.Values{"table_name": {"change_request"}, "table_sys_id": {changeSysID}, "file_name": {name}}
	var out struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/now/attachment/file", q, contentType, data, &out); err != nil {
		return Attachment{}, err
	}
	a, ok := attachmentOf(out.Result, changeSysID, name)
	if !ok {
		return Attachment{}, fmt.Errorf("POST /api/now/attachment/file: %w", &StatusError{Status: http.StatusCreated,
			Message: "the answer does not name the attachment of " + name + " on the change request"})
	}
	return a, nil
}

// FindAttachment returns the change request's attachment with the file name, nil when it has none:
// the one an upload made although its answer was lost. Only an exact match counts.
func (c *Client) FindAttachment(ctx context.Context, changeSysID, name string) (*Attachment, error) {
	if !SysIDRe.MatchString(changeSysID) {
		return nil, fmt.Errorf("%q is not a sys_id", changeSysID)
	}
	if !fileNameRe.MatchString(name) {
		return nil, fmt.Errorf("%q is not a file name Quai attaches", name)
	}
	q := url.Values{"sysparm_query": {"table_name=change_request^table_sys_id=" + changeSysID + "^file_name=" + name}, "sysparm_limit": {"10"}}
	var out struct {
		Result []map[string]json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/now/attachment", q, nil, &out); err != nil {
		return nil, err
	}
	for _, raw := range out.Result {
		if a, ok := attachmentOf(raw, changeSysID, name); ok {
			return &a, nil
		}
	}
	return nil, nil
}

// attachmentOf reads an attachment's metadata, and whether it is the file name's on the change
// request.
func attachmentOf(raw map[string]json.RawMessage, changeSysID, name string) (Attachment, bool) {
	sysID, _ := fieldOf(raw, "sys_id")
	table, _ := fieldOf(raw, "table_name")
	record, _ := fieldOf(raw, "table_sys_id")
	file, _ := fieldOf(raw, "file_name")
	if !SysIDRe.MatchString(sysID) || table != "change_request" || record != changeSysID || file != name {
		return Attachment{}, false
	}
	a := Attachment{SysID: sysID, FileName: file, Size: -1}
	if s, _ := fieldOf(raw, "size_bytes"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
			a.Size = n
		}
	}
	return a, true
}

// ValidNumber reports whether s is a record's number as ServiceNow makes them, a prefix and digits
// (INC0012345, CHG0030001): the only incident reference Quai queries.
func ValidNumber(s string) bool { return numberRe.MatchString(s) }

// Incident is an incident as the Table API reports it.
type Incident struct {
	SysID, Number string
	// Active is its active flag: "true" while it is open, "false" once it is closed or canceled.
	Active string
	// State is its state's value (1 New, 2 In Progress, 3 On Hold, 6 Resolved, 7 Closed, 8 Canceled).
	State string
	// Change is the change request its rfc field names (the change request's "Incidents Fixed By
	// Change") when it is a sys_id; HasChange whether it names one at all.
	Change    string
	HasChange bool
}

var (
	// ErrNoIncident is an incident number ServiceNow answers it has no incident of.
	ErrNoIncident = errors.New("ServiceNow has no incident with this number")
	// ErrIncidentAmbiguous is two incidents with one number.
	ErrIncidentAmbiguous = errors.New("ServiceNow holds more than one incident with this number")
	// ErrUnclear is an answer that says neither yes nor no: a result missing, or of other incidents
	// than the one asked for.
	ErrUnclear = errors.New("ServiceNow's answer does not say whether the incident exists")
)

const incidentFields = "sys_id,number,active,state,rfc"

// FindIncident returns the incident with the number, which must be ValidNumber: queried exactly
// (number=<number>), then only an exact match counts. ErrNoIncident when ServiceNow answers none,
// ErrIncidentAmbiguous for two, ErrUnclear for an answer that is neither.
func (c *Client) FindIncident(ctx context.Context, number string) (Incident, error) {
	if !numberRe.MatchString(number) {
		return Incident{}, fmt.Errorf("%q is not an incident number Quai queries", number)
	}
	q := url.Values{"sysparm_query": {"number=" + number}, "sysparm_limit": {"10"}, "sysparm_fields": {incidentFields},
		"sysparm_exclude_reference_link": {"true"}}
	var out struct {
		Result *[]map[string]json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/now/table/incident", q, nil, &out); err != nil {
		return Incident{}, err
	}
	if out.Result == nil {
		return Incident{}, ErrUnclear
	}
	if len(*out.Result) == 0 {
		return Incident{}, ErrNoIncident
	}
	var found []Incident
	for _, raw := range *out.Result {
		inc, ok := incidentOf(raw)
		if !ok {
			return Incident{}, ErrUnclear
		}
		if inc.Number == number {
			found = append(found, inc)
		}
	}
	switch len(found) {
	case 0:
		return Incident{}, ErrUnclear
	case 1:
		return found[0], nil
	}
	return Incident{}, ErrIncidentAmbiguous
}

// GetIncident reads an incident by its sys_id.
func (c *Client) GetIncident(ctx context.Context, sysID string) (Incident, error) {
	if !SysIDRe.MatchString(sysID) {
		return Incident{}, fmt.Errorf("%q is not a sys_id", sysID)
	}
	q := url.Values{"sysparm_fields": {incidentFields}, "sysparm_exclude_reference_link": {"true"}}
	return c.incident(ctx, http.MethodGet, sysID, q, nil)
}

// LinkIncident sets an incident's rfc field to a change request: the change request lists it in
// its "Incidents Fixed By Change". The answer must say so.
func (c *Client) LinkIncident(ctx context.Context, sysID, changeSysID string) (Incident, error) {
	if !SysIDRe.MatchString(sysID) || !SysIDRe.MatchString(changeSysID) {
		return Incident{}, fmt.Errorf("%q or %q is not a sys_id", sysID, changeSysID)
	}
	q := url.Values{"sysparm_fields": {incidentFields}, "sysparm_exclude_reference_link": {"true"}}
	inc, err := c.incident(ctx, http.MethodPatch, sysID, q, map[string]string{"rfc": changeSysID})
	if err == nil && inc.Change != changeSysID {
		return Incident{}, fmt.Errorf("PATCH /api/now/table/incident: %w", &StatusError{Status: http.StatusOK,
			Message: "the answer does not name the change request in the incident's rfc"})
	}
	return inc, err
}

func (c *Client) incident(ctx context.Context, method, sysID string, q url.Values, body any) (Incident, error) {
	var out struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, method, "/api/now/table/incident/"+sysID, q, body, &out); err != nil {
		return Incident{}, err
	}
	inc, ok := incidentOf(out.Result)
	if !ok || inc.SysID != sysID {
		return Incident{}, fmt.Errorf("ServiceNow answered another incident than %s", sysID)
	}
	return inc, nil
}

// incidentOf reads an incident of an answer, and whether it has a valid sys_id and number; its
// rfc only when it is a sys_id.
func incidentOf(raw map[string]json.RawMessage) (Incident, bool) {
	var inc Incident
	inc.SysID, _ = fieldOf(raw, "sys_id")
	inc.Number, _ = fieldOf(raw, "number")
	if !SysIDRe.MatchString(inc.SysID) || !numberRe.MatchString(inc.Number) {
		return Incident{}, false
	}
	inc.Active, _ = fieldOf(raw, "active")
	inc.State, _ = fieldOf(raw, "state")
	inc.Active, inc.State = Text(inc.Active, 10), Text(number(inc.State), 10)
	if rfc, _ := fieldOf(raw, "rfc"); rfc != "" {
		inc.HasChange = true
		if SysIDRe.MatchString(rfc) {
			inc.Change = rfc
		}
	}
	return inc, true
}

// IncidentLink is the address of an incident's form in the instance, from the configured root and a
// sys_id Quai checked: never a link an answer gave.
func (c *Client) IncidentLink(sysID string) string {
	if !SysIDRe.MatchString(sysID) {
		return ""
	}
	return c.cfg.InstanceURL + "/incident.do?sys_id=" + sysID
}

// change calls the Change Management API and reads the change request it answers.
func (c *Client) change(ctx context.Context, method, path string, q url.Values, body any) (Record, error) {
	var out struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, method, path, q, body, &out); err != nil {
		return Record{}, err
	}
	return record(out.Result)
}

// Parse reads a change request of an answer's result: each field a {value, display_value} pair
// (the Change Management API) or a plain value (the Table API); a value a string or a number.
func Parse(b []byte) (Record, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return Record{}, err
	}
	return record(raw)
}

// fieldOf is a field of an answer's record: a {value, display_value} pair (the Change Management
// API), a {link, value} reference (the Table API without sysparm_exclude_reference_link), or a
// plain value; its value and display value. A link is never read.
func fieldOf(raw map[string]json.RawMessage, name string) (string, string) {
	v, ok := raw[name]
	if !ok {
		return "", ""
	}
	var pair struct {
		Value   json.RawMessage `json:"value"`
		Display json.RawMessage `json:"display_value"`
	}
	if json.Unmarshal(v, &pair) == nil && pair.Value != nil {
		return scalar(pair.Value), scalar(pair.Display)
	}
	return scalar(v), ""
}

func record(raw map[string]json.RawMessage) (Record, error) {
	if raw == nil {
		return Record{}, errors.New("ServiceNow answered no change request")
	}
	field := func(name string) (string, string) { return fieldOf(raw, name) }
	var r Record
	r.SysID, _ = field("sys_id")
	if !SysIDRe.MatchString(r.SysID) {
		return Record{}, errors.New("ServiceNow answered a change request without a valid sys_id")
	}
	if r.Number, _ = field("number"); !numberRe.MatchString(r.Number) {
		r.Number = ""
	}
	r.Type, _ = field("type")
	var label string
	r.State, label = field("state")
	r.State = number(r.State)
	if states[r.State] != "" {
		r.StateLabel = Text(label, 40)
	}
	r.Approval, _ = field("approval")
	r.CorrelationID, _ = field("correlation_id")
	r.CloseCode, _ = field("close_code")
	if m, ok := raw["__meta"]; ok {
		var meta struct {
			Ignored []string `json:"ignoredFields"`
		}
		_ = json.Unmarshal(m, &meta)
		r.Ignored = meta.Ignored
	}
	r.Type, r.Approval, r.CloseCode = Text(r.Type, 40), Text(r.Approval, 40), Text(r.CloseCode, 40)
	return r, nil
}

// scalar is a JSON string's text, a number's digits or a boolean's true or false; "" for anything
// else.
func scalar(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(v, &n) == nil {
		return n.String()
	}
	var b bool
	if json.Unmarshal(v, &b) == nil {
		return strconv.FormatBool(b)
	}
	return ""
}

// number writes an integral number as ServiceNow's choice values are ("3.0" is "3"); anything
// else as it is.
func number(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1e6 {
		return s
	}
	return strconv.Itoa(int(f))
}

// call makes one call to the instance with the service account's credentials: once more with a
// new token when it is refused (401), and again after a wait when throttled (429, 503), within
// maxWait.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	if body == nil {
		return c.do(ctx, method, path, q, "", nil, out)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, q, "application/json", b, out)
}

// do is call with a body of contentType as it is: none when contentType is "".
func (c *Client) do(ctx context.Context, method, path string, q url.Values, contentType string, b []byte, out any) error {
	u := c.cfg.InstanceURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	renewed := false
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		token, err := c.authorize(ctx, req)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // the URL holds the fields: the error names the path instead
			}
			return &UnreachableError{Err: redact(fmt.Errorf("%s %s: %w", method, path, err), token)}
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
		_ = resp.Body.Close()
		switch {
		case rerr != nil:
			return &UnreachableError{Err: fmt.Errorf("%s %s: reading the answer: %w", method, path, rerr)}
		case len(data) > maxAnswer:
			return fmt.Errorf("%s %s: an answer over %d bytes", method, path, maxAnswer)
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil {
				if err := json.Unmarshal(data, out); err != nil {
					return fmt.Errorf("%s %s: an answer that is not JSON", method, path)
				}
			}
			return nil
		case resp.StatusCode == http.StatusUnauthorized && c.cfg.OAuth != nil && !renewed:
			// Revoked or expired early: a new token, once.
			c.forget(token)
			renewed = true
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			if wait, ok := retryWait(ctx, resp.Header, attempt, c.now()); ok {
				if err := c.sleep(ctx, wait); err != nil {
					return err
				}
				continue
			}
		}
		e := &StatusError{Status: resp.StatusCode, Message: message(data)}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return &UnreachableError{Err: fmt.Errorf("%s %s: %w", method, path, e)}
		}
		return fmt.Errorf("%s %s: %w", method, path, e)
	}
}

// retryWait says whether to try a throttled call again, and after how long: what its answer's
// Retry-After says, or an exponential backoff with jitter, within maxWait and the deadline, at
// most maxRetries times.
func retryWait(ctx context.Context, h http.Header, attempt int, now time.Time) (time.Duration, bool) {
	if attempt >= maxRetries {
		return 0, false
	}
	wait := time.Second<<attempt + rand.N(500*time.Millisecond) // #nosec G404 -- jitter, not a secret
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			wait = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(s); err == nil {
			wait = at.Sub(now)
		}
	}
	if wait > maxWait {
		return 0, false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait+5*time.Second {
		return 0, false
	}
	return max(wait, 0), true
}

// message is an error answer's message ({"error": {"message", "detail"}}), cut and neutral.
func message(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	m := e.Error.Message
	if e.Error.Detail != "" {
		m += ": " + e.Error.Detail
	}
	return oneLine(m, 200)
}

// authorize sets the request's credentials, and returns the token it carries ("" with basic).
func (c *Client) authorize(ctx context.Context, req *http.Request) (string, error) {
	if b := c.cfg.Basic; b != nil {
		s, err := c.secrets.Resolve(ctx, b.Password.Ref)
		if err != nil {
			return "", fmt.Errorf("the ServiceNow service account's password: %w", err) // names the reference, never the value
		}
		req.SetBasicAuth(b.Username, s.Reveal())
		return "", nil
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return token, nil
}

// bearer returns the OAuth application's token: the cached one until tokenMargin before it
// expires, a new one otherwise, from the instance's token endpoint.
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expiry.Add(-tokenMargin)) {
		return c.token, nil
	}
	o := c.cfg.OAuth
	s, err := c.secrets.Resolve(ctx, o.ClientSecret.Ref)
	if err != nil {
		return "", fmt.Errorf("the ServiceNow OAuth client secret: %w", err)
	}
	secret := s.Reveal()
	cc := clientcredentials.Config{ClientID: o.ClientID, ClientSecret: secret, TokenURL: c.cfg.InstanceURL + "/oauth_token.do",
		AuthStyle: oauth2.AuthStyleInParams}
	fetched, started := c.now(), time.Now()
	tok, err := cc.Token(context.WithValue(ctx, oauth2.HTTPClient, c.http))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			e := fmt.Errorf("the ServiceNow token: HTTP %d %s", re.Response.StatusCode, oneLine(re.ErrorCode, 60))
			if re.Response.StatusCode >= 500 || re.Response.StatusCode == http.StatusTooManyRequests {
				return "", &UnreachableError{Err: e}
			}
			return "", e
		}
		return "", &UnreachableError{Err: redact(fmt.Errorf("the ServiceNow token: %w", unwrapURL(err)), secret)}
	}
	if tok.AccessToken == "" || tok.Expiry.IsZero() {
		return "", errors.New("the ServiceNow token: the answer has no token or expiry")
	}
	// oauth2 sets Expiry from expires_in by the system's clock; the cache keeps it by Quai's.
	c.token, c.expiry = tok.AccessToken, fetched.Add(tok.Expiry.Sub(started))
	return c.token, nil
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// forget drops the cached token when it is the one refused.
func (c *Client) forget(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token, c.expiry = "", time.Time{}
	}
}

// redact removes a credential from an error's text.
func redact(err error, secret string) error {
	if err == nil || secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}

// codeTag is ServiceNow's [code] and [/code], which a journal field (work notes, comments)
// renders as HTML between them, in any case and with spaces.
var codeTag = regexp.MustCompile(`(?i)\[\s*(/?)\s*code\s*\]`)

// Text makes s neutral for a field of ServiceNow and cuts it to max characters: [code] tags are
// broken ("(code)"), &, < and > are escaped, so that nothing Quai writes is markup; control
// characters but line breaks and tabs are dropped, as are invalid UTF-8, U+2028, U+2029 and
// bidirectional controls; an ellipsis ends a cut text.
func Text(s string, max int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\r\n", "\n"), "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), r == '\u2028', r == '\u2029', unicode.Is(unicode.Bidi_Control, r):
			return -1
		}
		return r
	}, s)
	for codeTag.MatchString(s) {
		s = codeTag.ReplaceAllString(s, "(${1}code)")
	}
	var pieces []string
	total := 0
	for _, r := range s {
		piece := string(r)
		switch r {
		case '&':
			piece = "&amp;"
		case '<':
			piece = "&lt;"
		case '>':
			piece = "&gt;"
		}
		pieces = append(pieces, piece)
		total += utf8.RuneCountInString(piece)
	}
	if total <= max {
		return strings.Join(pieces, "")
	}
	var b strings.Builder
	n := 0
	for _, p := range pieces {
		c := utf8.RuneCountInString(p)
		if n+c > max-1 {
			break
		}
		b.WriteString(p)
		n += c
	}
	if max > 0 {
		b.WriteString("…")
	}
	return b.String()
}

// oneLine is s on one line, neutral, cut to max characters.
func oneLine(s string, max int) string {
	return Text(strings.Join(strings.Fields(s), " "), max)
}
