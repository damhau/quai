package notify

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	htmltemplate "html/template"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode"
)

// message returns a notification as an email: a text part and an HTML part, the HTML made with
// html/template, which escapes what people wrote (a reason, a name). Every header is one line:
// names, titles and subjects come from people and the identity provider, so control characters,
// line breaks among them, become spaces, and non-ASCII text is encoded (RFC 2047).
func (e *Email) message(to Recipient, m Message) ([]byte, error) {
	var text, html bytes.Buffer
	if err := textTemplate.Execute(&text, m); err != nil {
		return nil, err
	}
	if err := htmlTemplate.Execute(&html, m); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	body := multipart.NewWriter(&b)
	for _, p := range []struct {
		contentType string
		content     []byte
	}{{"text/plain; charset=utf-8", text.Bytes()}, {"text/html; charset=utf-8", html.Bytes()}} {
		w, err := body.CreatePart(textproto.MIMEHeader{"Content-Type": {p.contentType}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write(p.content); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := body.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	header := func(name, value string) {
		out.WriteString(name + ": " + value + "\r\n")
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	header("From", e.from.String())
	header("To", (&mail.Address{Name: oneLine(to.Name, 100), Address: to.Address}).String())
	header("Subject", mime.QEncoding.Encode("utf-8", oneLine(m.Subject, 200)))
	header("Date", e.now().Format(time.RFC1123Z))
	header("Message-ID", "<"+hex.EncodeToString(id)+"@"+e.helo+">")
	header("MIME-Version", "1.0")
	header("Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": body.Boundary()}))
	header("Auto-Submitted", "auto-generated") // RFC 3834: no automatic replies
	header("X-Auto-Response-Suppress", "All")  // nor out-of-office replies from Exchange
	out.WriteString("\r\n")
	out.Write(b.Bytes())
	return out.Bytes(), nil
}

// oneLine makes s fit a header: control characters (line breaks among them) become spaces,
// spaces are collapsed, and it is cut at n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "..."
	}
	return s
}

var textTemplate = texttemplate.Must(texttemplate.New("text").Parse(`{{.Headline}}
{{with .Request}}
Request #{{.Number}}: {{.Title}}
Repository: {{.Repository}}
Requested by: {{.Requester}}
{{if .Reason}}Reason: {{.Reason}}
{{end}}{{if .Waiting}}Waiting for: {{range $i, $w := .Waiting}}{{if $i}}; {{end}}{{$w}}{{end}}
{{end}}{{if .Changes}}
Changes:
{{range .Changes}}  {{.Op}} {{.Path}} (+{{.Added}} -{{.Removed}})
{{end}}{{end}}{{range .Plans}}
Plan of {{.Stack}} ({{.Status}}): {{.Add}} to add, {{.Change}} to change, {{.Destroy}} to destroy{{if .Cost}}; {{.Cost}}{{end}}
{{range .Policies}}  Policy {{.Name}}: {{.Result}}
{{end}}{{range .Resources}}  {{.Action}} {{.Address}}
{{end}}{{if .More}}  and {{.More}} more
{{end}}{{end}}{{end}}{{with .Alert}}
Repository: {{.Repository}}
{{range .Details}}{{.Label}}: {{.Value}}
{{end}}{{end}}
Open it in Quai: {{.Link}}

--
Quai sent this message because of your role in this request or repository.
`))

var htmlTemplate = htmltemplate.Must(htmltemplate.New("html").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Subject}}</title></head>
<body style="font-family: -apple-system, 'Segoe UI', Helvetica, Arial, sans-serif; font-size: 14px; color: #1f2328; margin: 0; padding: 16px;">
<p style="font-size: 16px; margin: 0 0 16px;">{{.Headline}}</p>
{{with .Request}}<table style="border-collapse: collapse; margin-bottom: 16px;">
<tr><td style="color: #59636e; padding: 2px 12px 2px 0;">Request</td><td>#{{.Number}} {{.Title}}</td></tr>
<tr><td style="color: #59636e; padding: 2px 12px 2px 0;">Repository</td><td>{{.Repository}}</td></tr>
<tr><td style="color: #59636e; padding: 2px 12px 2px 0;">Requested by</td><td>{{.Requester}}</td></tr>
{{if .Reason}}<tr><td style="color: #59636e; padding: 2px 12px 2px 0; vertical-align: top;">Reason</td><td style="white-space: pre-wrap;">{{.Reason}}</td></tr>
{{end}}{{if .Waiting}}<tr><td style="color: #59636e; padding: 2px 12px 2px 0; vertical-align: top;">Waiting for</td><td>{{range $i, $w := .Waiting}}{{if $i}}; {{end}}{{$w}}{{end}}</td></tr>
{{end}}</table>
{{if .Changes}}<p style="margin: 0 0 4px; font-weight: 600;">Changes</p>
<ul style="margin: 0 0 16px; padding-left: 20px; font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px;">
{{range .Changes}}<li>{{.Op}} {{.Path}} <span style="color: #1a7f37;">+{{.Added}}</span> <span style="color: #d1242f;">-{{.Removed}}</span></li>
{{end}}</ul>
{{end}}{{range .Plans}}<p style="margin: 0 0 4px; font-weight: 600;">Plan of {{.Stack}} ({{.Status}})</p>
<p style="margin: 0 0 4px;">{{.Add}} to add, {{.Change}} to change, {{.Destroy}} to destroy{{if .Cost}}; {{.Cost}}{{end}}</p>
{{if .Policies}}<p style="margin: 0 0 4px;">{{range $i, $p := .Policies}}{{if $i}}, {{end}}Policy {{$p.Name}}: {{$p.Result}}{{end}}</p>
{{end}}{{if .Resources}}<ul style="margin: 0 0 16px; padding-left: 20px; font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px;">
{{range .Resources}}<li>{{.Action}} {{.Address}}</li>
{{end}}{{if .More}}<li>and {{.More}} more</li>
{{end}}</ul>
{{end}}{{end}}{{end}}{{with .Alert}}<table style="border-collapse: collapse; margin-bottom: 16px;">
<tr><td style="color: #59636e; padding: 2px 12px 2px 0;">Repository</td><td>{{.Repository}}</td></tr>
{{range .Details}}<tr><td style="color: #59636e; padding: 2px 12px 2px 0; vertical-align: top;">{{.Label}}</td><td>{{.Value}}</td></tr>
{{end}}</table>
{{end}}<p style="margin: 16px 0;"><a href="{{.Link}}" style="background: #1f2328; color: #ffffff; padding: 8px 14px; border-radius: 6px; text-decoration: none;">Open it in Quai</a></p>
<p style="color: #59636e; font-size: 12px; margin: 24px 0 0;">Quai sent this message because of your role in this request or repository.</p>
</body>
</html>
`))
