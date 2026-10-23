package q2spot

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEscape(t *testing.T) {
	in := "100%\tA\nB\rC\x00é"
	if e := Escape(in); e != "100%25%09A%0AB%0DCé" {
		t.Fatalf("escape %q", e)
	}
	if u, err := Unescape("100%25%09A%0aB%0DC"); err != nil || u != "100%\tA\nB\rC" {
		t.Fatalf("unescape %q %v", u, err)
	}
	for _, bad := range []string{"%", "%2", "%zz", "%00"} {
		if _, err := Unescape(bad); err == nil {
			t.Errorf("%q unescaped", bad)
		}
	}
}

func TestCut(t *testing.T) {
	if c := Cut("aé", 2); c != "a" {
		t.Fatalf("%q", c)
	}
	if c := Cut("abc", 5); c != "abc" {
		t.Fatalf("%q", c)
	}
}

func TestRequest(t *testing.T) {
	r, err := ParseRequest([]byte("Q2SPOT/1\n7\tsearch\ttracks\tAC%09DC\n"))
	if err != nil || r.ID != "7" || r.Op != "search" || r.Arg(0) != "tracks" || r.Arg(1) != "AC\tDC" || r.Arg(2) != "" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"Q2SPOT/2\n1\tx\n", "Q2SPOT/1\n1\n", "Q2SPOT/1\n1\tx\n\n", "Q2SPOT/1\n1\tx%\n"} {
		if _, err := ParseRequest([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestEncodeLimits(t *testing.T) {
	var rows []Row
	for i := 0; i < 60; i++ {
		rows = append(rows, Row{Kind: "track", URI: "u", Title: strings.Repeat("é", 200)})
	}
	b, kept := Reply{Rows: rows}.Encode("3")
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if kept != MaxRows || len(lines) != 2+MaxRows || lines[1] != "3\tok\t\t" || len(b) > MaxReply {
		t.Fatalf("kept %d, %d lines, %q", kept, len(lines), lines[1])
	}
	if f := strings.Split(lines[2], "\t"); len(f) != 6 || len(f[2]) != 254 {
		t.Fatalf("title %d bytes", len(f[2]))
	}
	big := make([]Row, 50)
	for i := range big {
		big[i] = Row{Kind: "track", URI: strings.Repeat("u", 2000)}
	}
	b, kept = Reply{Rows: big}.Encode("1")
	if len(b) > MaxReply || kept >= 50 || kept == 0 {
		t.Fatalf("%d bytes, kept %d", len(b), kept)
	}
}

func TestServe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	go Serve(path, func(r Request) Reply {
		return Reply{Message: r.Op, Rows: []Row{{Kind: "info", Title: r.Arg(0)}}}
	})
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("Q2SPOT/1\n9\tmenu\tA%25\n"))
	c.(*net.UnixConn).CloseWrite()
	buf := make([]byte, 1024)
	n, _ := c.Read(buf)
	if got := string(buf[:n]); got != "Q2SPOT/1\n9\tok\tmenu\t\ninfo\t\tA%25\t\t\t\n" {
		t.Fatalf("%q", got)
	}
}
