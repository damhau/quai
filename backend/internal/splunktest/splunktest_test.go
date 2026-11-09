package splunktest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// post sends body to the collector with an Authorization header (none when empty), and returns
// the status and the answer.
func post(t *testing.T, s *Server, auth, body string) (int, Reply) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.URL+Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var r Reply
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("answer %q: %v", b, err)
	}
	return resp.StatusCode, r
}

// TestCollector: the documented answers, and the events taken in order, one after the other or
// in an array.
func TestCollector(t *testing.T) {
	s := New(t)
	s.Indexes = []string{"audit"}
	token := "Splunk " + s.Token
	n := func(i int) *int { return &i }
	for _, c := range []struct {
		name, auth, body string
		status           int
		want             Reply
	}{
		{"no token", "", `{"event":"x"}`, 401, TokenRequired},
		{"basic", "Basic eDp5", `{"event":"x"}`, 401, InvalidAuth},
		{"wrong token", "Splunk nope", `{"event":"x"}`, 403, InvalidToken},
		{"no data", token, "  ", 400, NoData},
		{"bad JSON", token, `{"event":"x"} {"event":`, 400, Reply{Text: "Invalid data format", Code: 6, Event: n(1)}},
		{"no event", token, `{"event":"x"}{"time":1}`, 400, Reply{Text: "Event field is required", Code: 12, Event: n(1)}},
		{"blank event", token, `{"event":""}`, 400, Reply{Text: "Event field cannot be blank", Code: 13, Event: n(0)}},
		{"other index", token, `[{"event":"x","index":"main"}]`, 400, Reply{Text: "Incorrect index", Code: 7, Event: n(0)}},
		{"one by one", token, `{"event":{"seq":1},"time":1761471612.331,"index":"audit"}` + "\n" + `{"event":{"seq":2}}`, 200, Success},
		{"in an array", token, `[{"event":{"seq":3}}, {"event":"four","sourcetype":"quai:audit"}]`, 200, Success},
	} {
		status, got := post(t, s, c.auth, c.body)
		if status != c.status || got.Text != c.want.Text || got.Code != c.want.Code || (c.want.Event != nil) != (got.Event != nil) ||
			(got.Event != nil && *got.Event != *c.want.Event) {
			t.Errorf("%s: %d %+v, want %d %+v", c.name, status, got, c.status, c.want)
		}
	}
	got := s.Events()
	if len(got) != 4 || string(got[0].Event) != `{"seq":1}` || got[0].Time != "1761471612.331" || got[0].Index != "audit" ||
		string(got[3].Event) != `"four"` || got[3].Sourcetype != "quai:audit" {
		t.Errorf("events = %+v", got)
	}

	s.Answer(&ServerBusy)
	if status, r := post(t, s, token, `{"event":"x"}`); status != 503 || r != ServerBusy {
		t.Errorf("busy: %d %+v", status, r)
	}
	s.Answer(&QueueAtCapacity)
	if status, r := post(t, s, token, `{"event":"x"}`); status != 429 || r.Code != 26 {
		t.Errorf("at capacity: %d %+v", status, r)
	}
	s.Answer(&TokenDisabled)
	if status, r := post(t, s, token, `{"event":"x"}`); status != 403 || r != TokenDisabled {
		t.Errorf("token disabled: %d %+v", status, r)
	}
	s.Answer(nil)
	if status, _ := post(t, s, token, `{"event":"x"}`); status != 200 || len(s.Events()) != 5 {
		t.Errorf("back: %d, %d events", status, len(s.Events()))
	}

	resp, err := s.Client().Get(s.URL + Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %s", resp.Status)
	}
}
