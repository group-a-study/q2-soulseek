package slsk

import (
	"encoding/binary"
	"net"
	"sync"
)

// FakeServer is enough of the Soulseek server for clients on the loopback, for tests and to try the
// phone page locally: logins, ports,
// addresses, ConnectToPeer and CantConnectToPeer forwarded, and searches sent on to every other
// user as the server's own FileSearch.
type FakeServer struct {
	l       net.Listener
	mu      sync.Mutex
	users   map[string]net.Conn
	ports   map[string]uint32
	badAddr map[string]bool // GetPeerAddress answers a dead port: only the indirect way works
	sent    map[string][]uint32
}

// NewFakeServer listens on addr ("127.0.0.1:0" for any port).
func NewFakeServer(addr string) (*FakeServer, error) {
	l, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, err
	}
	s := &FakeServer{l: l, users: map[string]net.Conn{}, ports: map[string]uint32{}, badAddr: map[string]bool{},
		sent: map[string][]uint32{}}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s, nil
}

func (s *FakeServer) Addr() string { return s.l.Addr().String() }
func (s *FakeServer) Close()       { s.l.Close() }

// BadAddress makes GetPeerAddress give user a dead port, so only indirect connections reach them.
func (s *FakeServer) BadAddress(user string) {
	s.mu.Lock()
	s.badAddr[user] = true
	s.mu.Unlock()
}

func (s *FakeServer) send(user string, m *msg) {
	s.mu.Lock()
	c := s.users[user]
	s.mu.Unlock()
	if c != nil {
		c.Write(m.frame())
	}
}

func (s *FakeServer) serve(c net.Conn) {
	defer c.Close()
	me := ""
	for {
		body, err := readFrame(c, 1<<20)
		if err != nil {
			return
		}
		code := binary.LittleEndian.Uint32(body)
		r := &reader{b: body[4:]}
		s.mu.Lock()
		s.sent[me] = append(s.sent[me], code)
		s.mu.Unlock()
		switch code {
		case sLogin:
			me = r.str()
			s.mu.Lock()
			s.users[me] = c
			s.mu.Unlock()
			c.Write(coded(sLogin).boolean(true).str("hi").u32(0x7f000001).str("x").boolean(false).frame())
		case sSetWaitPort:
			s.mu.Lock()
			s.ports[me] = r.u32()
			s.mu.Unlock()
		case sGetPeerAddress:
			user := r.str()
			s.mu.Lock()
			port := s.ports[user]
			if s.badAddr[user] {
				port = 1
			}
			s.mu.Unlock()
			c.Write(coded(sGetPeerAddress).str(user).u32(0x7f000001).u32(port).u32(0).u32(0).frame())
		case sConnectToPeer:
			token, user, typ := r.u32(), r.str(), r.str()
			s.mu.Lock()
			port := s.ports[me]
			s.mu.Unlock()
			s.send(user, coded(sConnectToPeer).str(me).str(typ).u32(0x7f000001).u32(port).u32(token).
				boolean(false).u32(0).u32(0))
		case sFileSearch:
			token, query := r.u32(), r.str()
			s.mu.Lock()
			var others []string
			for u := range s.users {
				if u != me {
					others = append(others, u)
				}
			}
			s.mu.Unlock()
			for _, u := range others {
				s.send(u, coded(sFileSearch).str(me).u32(token).str(query))
			}
		case sCantConnectToPeer:
			token, user := r.u32(), r.str()
			s.send(user, coded(sCantConnectToPeer).u32(token))
		}
	}
}
