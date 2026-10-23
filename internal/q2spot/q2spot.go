// Package q2spot is Q2SPOT/1 (q2-pod's docs/librespot.md#browsing-protocol-q2spot1), the line protocol Q2
// Pod's browser speaks to a card add-on: one request per Unix stream connection, the client
// shutting its write side, the add-on answering and closing.
package q2spot

import (
	"errors"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Version    = "Q2SPOT/1"
	MaxRequest = 8 << 10
	MaxReply   = 64 << 10
	MaxRows    = 50
	MaxText    = 255 // a title or subtitle, cut on a character boundary
	MaxMessage = 120
	Timeout    = 15 * time.Second
)

// Statuses the browser knows (q2-pod's docs/librespot.md).
const (
	OK          = "ok"
	Login       = "login"
	Offline     = "offline"
	Premium     = "premium"
	Refused     = "refused"
	Unavailable = "unavailable"
	Throttled   = "throttled"
	Unsupported = "unsupported"
	Bad         = "bad"
	Error       = "error"
)

var ErrMalformed = errors.New("q2spot: malformed")

// Escape writes %, tab, LF and CR as %25, %09, %0A and %0D; every other byte is literal but NUL,
// which the browser takes as a malformed reply, so it is dropped.
func Escape(s string) string {
	if !strings.ContainsAny(s, "%\t\n\r\x00") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0:
		case '%', '\t', '\n', '\r':
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func hex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c|32 >= 'a' && c|32 <= 'f':
		return int(c|32-'a') + 10
	}
	return -1
}

// Unescape reverses Escape. A % without two hex digits, or an escaped NUL, is malformed.
func Unescape(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || hex(s[i+1]) < 0 || hex(s[i+2]) < 0 {
			return "", ErrMalformed
		}
		v := byte(hex(s[i+1])<<4 | hex(s[i+2]))
		if v == 0 {
			return "", ErrMalformed
		}
		b.WriteByte(v)
		i += 2
	}
	return b.String(), nil
}

// Cut shortens s to at most n bytes without splitting a UTF-8 character.
func Cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type Request struct {
	ID   string
	Op   string
	Args []string
}

// Arg is the i-th argument, "" when there are fewer.
func (r Request) Arg(i int) string {
	if i < len(r.Args) {
		return r.Args[i]
	}
	return ""
}

// ParseRequest reads "Q2SPOT/1\n<id>\t<op>\t<arg>…\n".
func ParseRequest(b []byte) (Request, error) {
	s := string(b)
	if len(b) > MaxRequest || !strings.HasPrefix(s, Version+"\n") || strings.IndexByte(s, 0) >= 0 {
		return Request{}, ErrMalformed
	}
	line := strings.TrimSuffix(s[len(Version)+1:], "\n")
	if strings.ContainsAny(line, "\n\r") {
		return Request{}, ErrMalformed
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return Request{}, ErrMalformed
	}
	for i, f := range fields {
		u, err := Unescape(f)
		if err != nil {
			return Request{}, err
		}
		fields[i] = u
	}
	return Request{ID: fields[0], Op: fields[1], Args: fields[2:]}, nil
}

type Row struct {
	Kind, URI, Title, Subtitle, Context, Position string
}

type Reply struct {
	Status, Message, Cursor string
	Rows                    []Row
}

// Fail is a reply with no rows.
func Fail(status, message string) Reply { return Reply{Status: status, Message: message} }

// Encode frames the reply for request id. Rows that would take it past MaxRows or MaxReply are left
// out; the count kept is returned so a paged handler can continue its cursor after the last one.
func (r Reply) Encode(id string) ([]byte, int) {
	if r.Status == "" {
		r.Status = OK
	}
	var b strings.Builder
	b.WriteString(Version + "\n")
	b.WriteString(Escape(id) + "\t" + Escape(r.Status) + "\t" + Escape(Cut(r.Message, MaxMessage)) + "\t" +
		Escape(r.Cursor) + "\n")
	kept := 0
	for _, row := range r.Rows {
		if kept == MaxRows {
			break
		}
		line := Escape(row.Kind) + "\t" + Escape(row.URI) + "\t" + Escape(Cut(row.Title, MaxText)) + "\t" +
			Escape(Cut(row.Subtitle, MaxText)) + "\t" + Escape(row.Context) + "\t" + Escape(row.Position) + "\n"
		if b.Len()+len(line) > MaxReply {
			break
		}
		b.WriteString(line)
		kept++
	}
	return []byte(b.String()), kept
}

// Handler answers one request.
type Handler func(Request) Reply

// Serve answers requests on a Unix stream socket at path until the listener fails. A stale socket
// file is replaced; the socket is made writable by everyone, as demo runs as root anyway.
func Serve(path string, h Handler) error {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	for {
		c, err := l.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go answer(c, h)
	}
}

func answer(c net.Conn, h Handler) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(Timeout))
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 4096)
	for len(buf) <= MaxRequest {
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	req, err := ParseRequest(buf)
	var out []byte
	if err != nil {
		out, _ = Fail(Bad, "Bad request").Encode(firstField(buf))
	} else {
		out, _ = h(req).Encode(req.ID)
	}
	c.Write(out)
}

// firstField is a malformed request's id, if it has a readable one, so the client can match the reply.
func firstField(b []byte) string {
	s := strings.TrimPrefix(string(b), Version+"\n")
	if i := strings.IndexAny(s, "\t\n"); i >= 0 {
		s = s[:i]
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "0"
		}
	}
	if s == "" {
		return "0"
	}
	return s
}
