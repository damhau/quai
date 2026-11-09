package siem

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"unicode/utf8"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
)

// syslogSender sends RFC 5424 messages over TLS (RFC 5425) or TCP, framed by octet counting
// (RFC 6587, section 3.4.1), a connection per batch.
//
// Syslog has no acknowledgment. The closest is the receiver closing the connection once it has
// read everything: after a batch, the sender closes its side and waits for the receiver to close
// its own. A receiver that dies first, or closes with data unread, resets the connection
// instead, and the batch is sent again.
type syslogSender struct {
	address  string
	tls      *tls.Config // nil over TCP
	facility int
	hostname string
}

// appName is the messages' APP-NAME.
const appName = "quai"

func newSyslog(c config.SyslogExport, network *netx.Network, hostname string) *syslogSender {
	s := &syslogSender{address: c.Address, facility: config.DefaultSyslogFacility, hostname: printable(hostname, 255)}
	if c.Facility != nil {
		s.facility = *c.Facility
	}
	if c.Transport != config.SyslogTCP {
		s.tls = network.TLSConfig()
	}
	return s
}

func (s *syslogSender) send(ctx context.Context, records []audit.Record) error {
	var frames bytes.Buffer
	for _, r := range records {
		msg, err := s.message(r)
		if err != nil {
			return err
		}
		frames.WriteString(strconv.Itoa(len(msg)))
		frames.WriteByte(' ')
		frames.Write(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var conn net.Conn
	var err error
	if s.tls != nil {
		conn, err = (&tls.Dialer{Config: s.tls}).DialContext(ctx, "tcp", s.address)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", s.address)
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := conn.Write(frames.Bytes()); err != nil {
		return err
	}
	var closeErr error
	switch c := conn.(type) {
	case *tls.Conn:
		closeErr = c.CloseWrite()
	case *net.TCPConn:
		closeErr = c.CloseWrite()
	}
	if closeErr != nil {
		return closeErr
	}
	// The receiver has read it all when it closes the connection. Over TLS, one that closes
	// without a close_notify ends with ErrUnexpectedEOF: closed all the same.
	if _, err := io.Copy(io.Discard, conn); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("the receiver did not close the connection after reading the messages: %w", err)
	}
	return nil
}

// message is a record as an RFC 5424 message: the event's time, this host, quai, the action as
// MSGID, no structured data, and the record in JSON as MSG, in ASCII (non-ASCII characters
// escaped), so that it needs no BOM.
func (s *syslogSender) message(r audit.Record) ([]byte, error) {
	j, err := recordJSON(r)
	if err != nil {
		return nil, err
	}
	pri := s.facility*8 + severity(r.Action)
	// The event's at is RFC 5424's TIMESTAMP already: RFC 3339, UTC, to the millisecond.
	header := fmt.Sprintf("<%d>1 %s %s %s - %s - ", pri, r.At, s.hostname, appName, printable(r.Action, 32))
	return append([]byte(header), asciiJSON(j)...), nil
}

// asciiJSON escapes the non-ASCII characters of JSON text, which can only be in its strings, as
// \u sequences (surrogate pairs beyond the BMP): the same value, in ASCII.
func asciiJSON(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r < utf8.RuneSelf:
			out = append(out, b[0])
		case r > 0xFFFF:
			r -= 0x10000
			out = fmt.Appendf(out, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			out = fmt.Appendf(out, `\u%04x`, r)
		}
		b = b[size:]
	}
	return out
}
