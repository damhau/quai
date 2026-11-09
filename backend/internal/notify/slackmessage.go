package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Slack's messages: Block Kit, built as Go values. Every text in a block is a plain_text object,
// which Slack shows as it is: no formatting, no mentions (<!channel>, <@U...>), no links
// (<https://...|...>), whatever people wrote. The message's text, its fallback for notifications,
// is formatted by Slack: &, < and > are escaped there.

type slackText struct {
	Type  string `json:"type"` // plain_text
	Text  string `json:"text"`
	Emoji bool   `json:"emoji"` // false: :name: stays text
}

type slackBlock struct {
	Type     string     `json:"type"` // header, section, context or actions
	Text     *slackText `json:"text,omitempty"`
	Elements []any      `json:"elements,omitempty"` // slackText in a context, slackButton in actions
}

type slackButton struct {
	Type     string    `json:"type"` // button
	Text     slackText `json:"text"`
	ActionID string    `json:"action_id"`
	URL      string    `json:"url"`
	Style    string    `json:"style,omitempty"` // primary or danger
}

// slackMessage is the arguments of chat.postMessage (no ts) and chat.update.
type slackMessage struct {
	Channel     string       `json:"channel"`
	TS          string       `json:"ts,omitempty"`
	Text        string       `json:"text"`
	Blocks      []slackBlock `json:"blocks"`
	UnfurlLinks *bool        `json:"unfurl_links,omitempty"`
	UnfurlMedia *bool        `json:"unfurl_media,omitempty"`
}

// Slack's limits, in characters (UTF-16 code units, to be safe): a header's text, a section's,
// a button's; and the blocks of a message.
const (
	slackHeaderLen  = 150
	slackSectionLen = 3000
	slackButtonLen  = 75
	slackMaxBlocks  = 50
)

// maxSlackBlocks is the size above which a message leaves its files and plans' resources out: what
// Slack sends back with a click stays under the interaction endpoint's limit
// (maxSlackInteraction).
const maxSlackBlocks = 24 * 1024

// slackPlans bounds the plans a message shows, a section each: a message has 50 blocks at most.
const slackPlans = 30

var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeMrkdwn escapes the characters Slack parses in text it formats, so that <!channel>,
// <@U...> and <https://...|...> show as written instead of acting.
func escapeMrkdwn(s string) string { return mrkdwnEscaper.Replace(s) }

// units is the length of s in UTF-16 code units.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += max(utf16.RuneLen(r), 1)
	}
	return n
}

// clipUnits shortens s to at most n UTF-16 code units, on a character boundary.
func clipUnits(s string, n int) string {
	if units(s) <= n {
		return s
	}
	u := 0
	for i, r := range s {
		ru := max(utf16.RuneLen(r), 1)
		if u+ru > n-3 {
			return s[:i] + "..."
		}
		u += ru
	}
	return s
}

func plain(s string) slackText { return slackText{Type: "plain_text", Text: s} }

// header is a message's title; a section when it is too long for a header.
func header(s string) slackBlock {
	s = oneLine(s, 600)
	if units(s) > slackHeaderLen {
		return section(s)
	}
	t := plain(s)
	return slackBlock{Type: "header", Text: &t}
}

// section is text of one or more lines.
func section(s string) slackBlock {
	if strings.TrimSpace(s) == "" {
		s = "-"
	}
	t := plain(clipUnits(s, slackSectionLen))
	return slackBlock{Type: "section", Text: &t}
}

// note is small, subtle text.
func note(s string) slackBlock {
	return slackBlock{Type: "context", Elements: []any{plain(clipUnits(s, slackSectionLen))}}
}

func button(label, actionID, url, style string) slackButton {
	return slackButton{Type: "button", Text: plain(clipUnits(label, slackButtonLen)), ActionID: actionID, URL: url, Style: style}
}

func buttons(bs ...slackButton) slackBlock {
	el := make([]any, len(bs))
	for i, b := range bs {
		el[i] = b
	}
	return slackBlock{Type: "actions", Elements: el}
}

// fallback is a message's text: one line, escaped.
func fallback(s string) string { return escapeMrkdwn(oneLine(s, 300)) }

// slackPersonal is a person's message: what the email says, in short, with Approve and Deny for
// an approval, and a link to the request.
func slackPersonal(m Message) (string, []slackBlock) {
	text := m.Subject
	if text == "" {
		text = strings.TrimSpace(m.Headline + " " + m.Status)
	}
	if m.Kind == DigestKind {
		return fallback(text), slackDigest(m)
	}
	b := slackFull(m, true)
	if j, err := json.Marshal(b); err == nil && len(j) > maxSlackBlocks {
		b = slackFull(m, false)
	}
	return fallback(text), b
}

func slackFull(m Message, details bool) []slackBlock {
	blocks := []slackBlock{header(m.Headline)}
	if m.Status != "" {
		blocks = append(blocks, section(oneLine(m.Status, 400)))
	}
	if rc := m.Request; rc != nil {
		lines := []string{fmt.Sprintf("Request: #%d %s", rc.Number, oneLine(rc.Title, 200)), "Repository: " + oneLine(rc.Repository, 200)}
		if !m.Closed {
			lines = append(lines, "Requested by: "+oneLine(rc.Requester, 200))
			if len(rc.Waiting) > 0 {
				lines = append(lines, "Waiting for: "+oneLine(strings.Join(rc.Waiting, "; "), 500))
			}
		}
		blocks = append(blocks, section(strings.Join(lines, "\n")))
		if !m.Closed {
			if rc.Reason != "" {
				blocks = append(blocks, section("Reason:\n"+lines20(rc.Reason)))
			}
			blocks = append(blocks, slackDetails(rc, details)...)
		}
	}
	if a := m.Alert; a != nil {
		lines := []string{"Repository: " + oneLine(a.Repository, 200)}
		for _, d := range a.Details {
			lines = append(lines, d.Label+": "+oneLine(d.Value, 300))
		}
		blocks = append(blocks, section(strings.Join(lines, "\n")))
	}
	var bs []slackButton
	if m.Approve != "" && m.Deny != "" && !m.Closed {
		blocks = append(blocks, note("Approve and Deny open the request in Quai with the decision ready: you confirm it there, signed in."))
		bs = append(bs, button("Approve", "approve", m.Approve, "primary"), button("Deny", "deny", m.Deny, "danger"))
	}
	bs = append(bs, button("Open in Quai", "open", m.Link, ""))
	if m.Preferences != "" && !m.Closed {
		bs = append(bs, button("Notification preferences", "preferences", m.Preferences, ""))
	}
	return append(blocks, buttons(bs...))
}

// lines20 is text of several lines, at most 20, each on one line.
func lines20(s string) string {
	const maxLines = 20
	lines := strings.Split(strings.ReplaceAll(clip(s, 2000), "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "...")
	}
	for i, l := range lines {
		lines[i] = oneLine(l, 500)
	}
	return strings.Join(lines, "\n")
}

// slackDetails lists a request's files and plans, a section for the files and one per plan: the
// counts always, the files and resources when details is true.
func slackDetails(rc *RequestContent, details bool) []slackBlock {
	var out []slackBlock
	if len(rc.Changes) > 0 {
		lines := []string{fmt.Sprintf("Changes (%d files)", len(rc.Changes))}
		if details {
			for i, c := range rc.Changes {
				if i == cardItems {
					lines = append(lines, fmt.Sprintf("and %d more", len(rc.Changes)-cardItems))
					break
				}
				lines = append(lines, fmt.Sprintf("%s %s (+%d -%d)", c.Op, oneLine(c.Path, 200), c.Added, c.Removed))
			}
		}
		out = append(out, section(strings.Join(lines, "\n")))
	}
	for i, p := range rc.Plans {
		if i == slackPlans {
			out = append(out, section(fmt.Sprintf("and %d more plans: the request page has them", len(rc.Plans)-slackPlans)))
			break
		}
		line := fmt.Sprintf("%d to add, %d to change, %d to destroy", p.Add, p.Change, p.Destroy)
		if p.Cost != "" {
			line += "; " + p.Cost
		}
		lines := []string{fmt.Sprintf("Plan of %s (%s)", oneLine(p.Stack, 100), oneLine(p.Status, 40)), line}
		for _, pol := range p.Policies {
			lines = append(lines, "Policy "+oneLine(pol.Name, 100)+": "+oneLine(pol.Result, 40))
		}
		if details {
			for j, r := range p.Resources {
				if j == cardItems {
					lines = append(lines, fmt.Sprintf("and %d more", len(p.Resources)-cardItems+p.More))
					break
				}
				lines = append(lines, oneLine(r.Action+" "+r.Address, 300))
			}
			if len(p.Resources) <= cardItems && p.More > 0 {
				lines = append(lines, fmt.Sprintf("and %d more", p.More))
			}
		}
		out = append(out, section(strings.Join(lines, "\n")))
	}
	return out
}

// slackDigest is a digest: a section per notification, at most 30.
func slackDigest(m Message) []slackBlock {
	blocks := []slackBlock{header(m.Subject), section(oneLine(m.Headline, 400))}
	for i, it := range m.Digest {
		if i == 30 {
			blocks = append(blocks, note(fmt.Sprintf("and %d more: the requests page has them", len(m.Digest)-30)))
			break
		}
		s := oneLine(it.Label, 60) + ": " + oneLine(it.Headline, 300)
		if it.Request != "" {
			s += "\n" + oneLine(it.Request, 300)
		}
		blocks = append(blocks, section(s))
	}
	bs := []slackButton{button("Open in Quai", "open", m.Link, "")}
	if m.Preferences != "" {
		bs = append(bs, button("Notification preferences", "preferences", m.Preferences, ""))
	}
	return append(blocks, buttons(bs...))
}

// slackChannelPost is a channel's post: identifiers, the state and a link; never what people
// wrote, a diff or a plan (open question 64).
func slackChannelPost(m Message) (string, []slackBlock) {
	blocks := []slackBlock{header(m.Headline)}
	var lines []string
	if rc := m.Request; rc != nil {
		lines = append(lines, fmt.Sprintf("Request: #%d", rc.Number), "Repository: "+oneLine(rc.Repository, 200))
		if rc.State != "" {
			lines = append(lines, "State: "+strings.ReplaceAll(rc.State, "_", " "))
		}
	}
	if a := m.Alert; a != nil {
		lines = append(lines, "Repository: "+oneLine(a.Repository, 200))
		for _, d := range a.Details {
			lines = append(lines, d.Label+": "+oneLine(d.Value, 300))
		}
	}
	if len(lines) > 0 {
		blocks = append(blocks, section(strings.Join(lines, "\n")))
	}
	return fallback(m.Headline), append(blocks, buttons(button("Open in Quai", "open", m.Link, "")))
}
