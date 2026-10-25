package slsk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// conn is a server, peer (P) or distributed (D) connection: framed messages, writes serialized.
type conn struct {
	c    *Client
	nc   net.Conn
	typ  byte // 'S', 'P', 'D'
	user string
	wmu  sync.Mutex
	once sync.Once
	dead chan struct{}
}

func newConn(c *Client, nc net.Conn, typ byte, user string) *conn {
	return &conn{c: c, nc: nc, typ: typ, user: user, dead: make(chan struct{})}
}

func (p *conn) write(b []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.nc.SetWriteDeadline(time.Now().Add(60 * time.Second))
	_, err := p.nc.Write(b)
	if err != nil {
		p.close()
	}
	return err
}

func (p *conn) send(m *msg) error { return p.write(m.frame()) }

func (p *conn) close() {
	p.once.Do(func() {
		close(p.dead)
		p.nc.Close()
	})
}

func (p *conn) alive() bool {
	select {
	case <-p.dead:
		return false
	default:
		return true
	}
}

type peerAddr struct{ ip, port uint32 }

func (a peerAddr) String() string {
	return fmt.Sprintf("%d.%d.%d.%d:%d", byte(a.ip>>24), byte(a.ip>>16), byte(a.ip>>8), byte(a.ip), a.port)
}

type peerCandidate struct {
	user     string
	ip, port uint32
}

// pending is one of our connection attempts to a user: the first established connection, direct or
// pierced from their end, is taken; the others are closed.
type pending struct {
	got   chan net.Conn
	fails chan struct{}
	mu    sync.Mutex
	done  bool
}

func (p *pending) deliver(nc net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		nc.Close()
		return
	}
	p.done = true
	p.got <- nc
}

func (p *pending) fail() {
	select {
	case p.fails <- struct{}{}:
	default:
	}
}

func (p *pending) finish() {
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
}

var errNoPeer = errors.New("can't connect to the user")
var errOffline = errors.New("the user is offline")

// peerAddress asks the server for a user's address; port 0 is an offline user.
func (c *Client) peerAddress(user string) (peerAddr, error) {
	ch := make(chan peerAddr, 1)
	c.mu.Lock()
	c.addrs[user] = append(c.addrs[user], ch)
	c.mu.Unlock()
	c.serverSend(coded(sGetPeerAddress).str(user))
	select {
	case a := <-ch:
		if a.port == 0 {
			return a, errOffline
		}
		return a, nil
	case <-time.After(15 * time.Second):
		return peerAddr{}, errNoPeer
	case <-c.stop:
		return peerAddr{}, errNoPeer
	}
}

// dial connects to user for a typ connection, as Soulseek's modern order has it: ConnectToPeer
// through the server (they connect to us and pierce with the token) and, at once, a direct
// connection to the address the server gives. The first to work is the connection.
func (c *Client) dial(user string, typ byte) (net.Conn, error) {
	c.mu.Lock()
	online := c.server != nil
	me := c.cfg.Username
	c.mu.Unlock()
	if !online {
		return nil, errors.New("not connected to the Soulseek server")
	}
	token := c.nextToken()
	p := &pending{got: make(chan net.Conn, 1), fails: make(chan struct{}, 2)}
	c.mu.Lock()
	c.pierce[token] = p
	c.mu.Unlock()
	defer func() {
		p.finish()
		c.mu.Lock()
		delete(c.pierce, token)
		c.mu.Unlock()
	}()
	c.serverSend(coded(sConnectToPeer).u32(token).str(user).str(string(typ)))
	offline := make(chan struct{}, 1)
	go func() {
		a, err := c.peerAddress(user)
		if err == errOffline {
			offline <- struct{}{}
			return
		}
		if err != nil {
			p.fail()
			return
		}
		nc, err := net.DialTimeout("tcp4", a.String(), 10*time.Second)
		if err != nil {
			p.fail()
			return
		}
		nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := nc.Write(coded8(iPeerInit).str(me).str(string(typ)).u32(0).frame()); err != nil {
			nc.Close()
			p.fail()
			return
		}
		nc.SetWriteDeadline(time.Time{})
		p.deliver(nc)
	}()
	deadline := time.After(30 * time.Second)
	fails := 0
	for {
		select {
		case nc := <-p.got:
			return nc, nil
		case <-offline:
			return nil, errOffline
		case <-p.fails:
			if fails++; fails == 2 {
				return nil, errNoPeer
			}
		case <-deadline:
			return nil, errNoPeer
		case <-c.stop:
			return nil, errNoPeer
		}
	}
}

// connectBack answers a ConnectToPeer: we connect to them and pierce with their token, and the
// connection then works as if they had opened it.
func (c *Client) connectBack(user string, typ byte, ip, port, token uint32) {
	nc, err := net.DialTimeout("tcp4", peerAddr{ip, port}.String(), 10*time.Second)
	if err == nil {
		nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err = nc.Write(coded8(iPierceFirewall).u32(token).frame())
		nc.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		if nc != nil {
			nc.Close()
		}
		c.serverSend(coded(sCantConnectToPeer).u32(token).str(user))
		return
	}
	c.route(user, typ, nc)
}

// incoming reads a connection's first message: a PierceFireWall answering one of our attempts, or
// a PeerInit opening a new one.
func (c *Client) incoming(nc net.Conn) {
	nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := readFrame(nc, 4096)
	if err != nil || len(body) < 1 {
		nc.Close()
		return
	}
	nc.SetReadDeadline(time.Time{})
	r := &reader{b: body[1:]}
	switch body[0] {
	case iPierceFirewall:
		token := r.u32()
		c.mu.Lock()
		p := c.pierce[token]
		c.mu.Unlock()
		if p == nil || r.err != nil {
			nc.Close()
			return
		}
		p.deliver(nc)
	case iPeerInit:
		user, typ := r.str(), r.str()
		if r.err != nil || len(typ) != 1 {
			nc.Close()
			return
		}
		c.route(user, typ[0], nc)
	default:
		nc.Close()
	}
}

// route serves a connection a user opened (or one we pierced for them).
func (c *Client) route(user string, typ byte, nc net.Conn) {
	switch typ {
	case 'P':
		c.servePeer(c.addPeer(user, nc))
	case 'F':
		c.fileIncoming(user, nc)
	default: // we take no distributed children
		nc.Close()
	}
}

// addPeer keeps nc as user's P connection; an older one is closed once it has nothing more to say.
func (c *Client) addPeer(user string, nc net.Conn) *conn {
	p := newConn(c, nc, 'P', user)
	c.mu.Lock()
	c.peers[user] = p
	c.mu.Unlock()
	return p
}

// peer is user's P connection, made if there is none.
func (c *Client) peer(user string) (*conn, error) {
	for {
		c.mu.Lock()
		p := c.peers[user]
		if p != nil && p.alive() {
			c.mu.Unlock()
			return p, nil
		}
		wait, busy := c.dialing[user]
		if !busy {
			wait = make(chan struct{})
			c.dialing[user] = wait
		}
		c.mu.Unlock()
		if busy {
			select {
			case <-wait:
				continue
			case <-time.After(45 * time.Second):
				return nil, errNoPeer
			}
		}
		nc, err := c.dial(user, 'P')
		c.mu.Lock()
		delete(c.dialing, user)
		close(wait)
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		p = c.addPeer(user, nc)
		go c.servePeer(p)
		return p, nil
	}
}

// sendPeer sends m to user over their P connection.
func (c *Client) sendPeer(user string, m *msg) error {
	p, err := c.peer(user)
	if err != nil {
		return err
	}
	return p.send(m)
}

// servePeer reads a P connection's messages until it closes or two minutes pass without one; a peer
// with more to say later (an upload ready for us) connects again.
func (c *Client) servePeer(p *conn) {
	defer func() {
		p.close()
		c.mu.Lock()
		if c.peers[p.user] == p {
			delete(c.peers, p.user)
		}
		c.mu.Unlock()
	}()
	for {
		p.nc.SetReadDeadline(time.Now().Add(2 * time.Minute))
		body, err := readFrame(p.nc, maxPeerMessage)
		if err != nil {
			return
		}
		if len(body) >= 4 {
			code := binary.LittleEndian.Uint32(body)
			c.peerMessage(p, code, &reader{b: body[4:]})
			// One that only brought search results is let go at once: a popular search brings
			// hundreds, and the Q2 has few files to keep open.
			if code == pSearchResponse && !c.dealing(p.user) {
				return
			}
		}
	}
}

// dealing is whether we have business with user that its P connection may carry: a transfer under
// way or waiting, or a folder asked for.
func (c *Client) dealing(user string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.folders) > 0 {
		return true
	}
	for _, l := range [][]*Transfer{c.downs, c.ups} {
		for _, t := range l {
			if t.User == user && t.State != Finished && t.State != Failed {
				return true
			}
		}
	}
	return false
}

func (c *Client) peerMessage(p *conn, code uint32, r *reader) {
	user := p.user
	switch code {
	case pShareListRequest:
		go c.sendShareList(p)
	case pSearchResponse:
		c.searchResponse(user, r)
	case pUserInfoRequest:
		c.mu.Lock()
		slots, queued, free := c.cfg.Slots, 0, c.shareActive
		active := 0
		for _, u := range c.ups {
			if u.State == Queued {
				queued++
			} else if u.State == Uploading || u.State == Starting || u.State == Requesting {
				active++
			}
		}
		free = free && active < slots
		c.mu.Unlock()
		p.send(coded(pUserInfoResponse).str("Sharing from a Shanling Q2 with Q2 Pod.").boolean(false).
			u32(uint32(slots)).u32(uint32(queued)).boolean(free).u32(0))
	case pFolderRequest:
		token, folder := r.u32(), r.str()
		if r.err == nil {
			go c.sendFolder(p, token, folder)
		}
	case pFolderResponse:
		c.folderResponse(r)
	case pTransferRequest:
		dir, token, file := r.u32(), r.u32(), r.str()
		size := uint64(0)
		if dir == 1 {
			size = r.u64()
		}
		if r.err == nil {
			c.transferRequest(p, dir, token, file, size)
		}
	case pTransferResponse:
		token, allowed := r.u32(), r.boolean()
		reason := ""
		if !allowed {
			reason = r.str()
		}
		c.transferResponse(token, allowed, reason)
	case pQueueUpload:
		if file := r.str(); r.err == nil {
			c.queueUpload(p, file)
		}
	case pPlaceInQueue:
		file, place := r.str(), r.u32()
		if r.err == nil {
			c.placeInQueue(user, file, int(place))
		}
	case pPlaceInQueueReq:
		if file := r.str(); r.err == nil {
			c.placeRequest(p, file)
		}
	case pUploadFailed:
		if file := r.str(); r.err == nil {
			c.uploadFailed(user, file)
		}
	case pUploadDenied:
		file, reason := r.str(), r.str()
		if r.err == nil {
			c.uploadDenied(user, file, reason)
		}
	default:
		if code > 1000 {
			log.Printf("peer %s: unknown message %d", Text(user), code)
		}
	}
}

// sendShareList answers a share list request: everything shared while sharing is active, else nothing.
func (c *Client) sendShareList(p *conn) {
	c.mu.Lock()
	var dirs []string
	var files [][]File
	if c.shareActive {
		dirs, files = c.shares.List()
	}
	c.mu.Unlock()
	m := &msg{}
	m.u32(uint32(len(dirs)))
	for i, d := range dirs {
		m.str(d)
		writeFiles(m, files[i])
	}
	m.u32(0).u32(0)
	p.send(coded(pShareListResponse).bytesRaw(compress(m.b)))
}

func (c *Client) sendFolder(p *conn, token uint32, folder string) {
	c.mu.Lock()
	var dirs map[string][]File
	if c.shareActive {
		dirs = c.shares.Folder(folder)
	}
	c.mu.Unlock()
	m := (&msg{}).u32(token).str(folder).u32(uint32(len(dirs)))
	for d, files := range dirs {
		m.str(d)
		writeFiles(m, files)
	}
	p.send(coded(pFolderResponse).bytesRaw(compress(m.b)))
}

func (m *msg) bytesRaw(b []byte) *msg { m.b = append(m.b, b...); return m }

// FolderFiles asks user for a folder's files, its subfolders' included, waiting up to timeout.
func (c *Client) FolderFiles(user, folder string, timeout time.Duration) (map[string][]File, error) {
	token := c.nextToken()
	ch := make(chan map[string][]File, 1)
	c.mu.Lock()
	c.folders[token] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.folders, token)
		c.mu.Unlock()
	}()
	if err := c.sendPeer(user, coded(pFolderRequest).u32(token).str(folder)); err != nil {
		return nil, err
	}
	select {
	case d := <-ch:
		return d, nil
	case <-time.After(timeout):
		return nil, errors.New("the user didn't send the folder")
	}
}

func (c *Client) folderResponse(r *reader) {
	raw, err := decompress(r.b, 32<<20)
	if err != nil {
		return
	}
	r = &reader{b: raw}
	token := r.u32()
	r.str()
	n := int(r.u32())
	dirs := map[string][]File{}
	for i := 0; i < n && i < 10000 && r.err == nil; i++ {
		d := r.str()
		dirs[d] = append(dirs[d], readFiles(r, 100000)...)
	}
	if r.err != nil {
		return
	}
	c.mu.Lock()
	ch := c.folders[token]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- dirs:
		default:
		}
	}
}
