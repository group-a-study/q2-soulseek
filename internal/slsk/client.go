package slsk

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/power"
)

// MajorVersion is the login version for experimental clients (SLSKPROTOCOL.md, Major Versions);
// MinorVersion is this client's own.
const (
	MajorVersion = 177
	MinorVersion = 1
)

// Sharing modes.
const (
	ShareOff      = "off"      // nothing shared, no search requests answered
	ShareCharging = "charging" // only while a cable powers the Q2
	ShareBattery  = "battery"  // also on battery while it is above BatteryMin percent
)

type Config struct {
	Server      string   `json:"server"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	Port        int      `json:"port"`
	Sharing     string   `json:"sharing"`
	BatteryMin  int      `json:"battery_min"`
	Shares      []string `json:"shares"`
	DownloadDir string   `json:"download_dir"`
	Slots       int      `json:"slots"`
	UploadKBps  int      `json:"upload_kbps"`
	MaxResults  int      `json:"max_results"`
}

// Defaults fills what a config leaves out.
func (c *Config) Defaults(card string) {
	if c.Server == "" {
		c.Server = "server.slsknet.org:2242"
	}
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = 2234
	}
	switch c.Sharing {
	case ShareOff, ShareCharging, ShareBattery:
	default:
		c.Sharing = ShareOff
	}
	if c.BatteryMin < 20 || c.BatteryMin > 100 {
		c.BatteryMin = 80
	}
	if c.DownloadDir == "" {
		c.DownloadDir = filepath.Join(card, "Soulseek")
	}
	if c.Slots <= 0 || c.Slots > 10 {
		c.Slots = 2
	}
	if c.UploadKBps < 0 {
		c.UploadKBps = 0
	}
	if c.MaxResults <= 0 || c.MaxResults > 500 {
		c.MaxResults = 100
	}
}

// Connection states.
const (
	StateSetup      = "setup"      // no account yet
	StateConnecting = "connecting" // dialling or logging in
	StateOnline     = "online"
	StateOffline    = "offline"   // the connection dropped; trying again
	StateRefused    = "refused"   // the server refused the login: Err says why
	StateElsewhere  = "elsewhere" // the account logged in somewhere else
	StateStopped    = "stopped"
)

// Client is one Soulseek session and its transfers. Its methods are safe for concurrent use.
type Client struct {
	dir string // the add-on's own folder: downloads.json, shares.cache

	mu       sync.Mutex
	cfg      Config
	state    string
	err      string
	server   *conn
	reconnAt time.Time
	retries  int
	listener net.Listener
	port     int
	token    uint32
	ourIP    uint32

	pierce  map[uint32]*pending        // our ConnectToPeer tokens, waiting for a PierceFireWall
	addrs   map[string][]chan peerAddr // GetPeerAddress replies awaited, by user
	peers   map[string]*conn           // the P connection to each user
	dialing map[string]chan struct{}   // a P connection being made, by user

	shares   *Shares
	scanning bool
	excluded []string

	searches []*Search
	nextID   int
	downs    []*Transfer
	ups      []*Transfer
	upTokens map[uint32]*Transfer // TransferRequests we sent, waiting for their TransferResponse
	folders  map[uint32]chan map[string][]File

	parent    *conn
	parentLvl uint32
	rootName  string
	findingP  bool

	pow          power.Status
	shareActive  bool
	uploadSpeed  float64
	bucket       *bucket
	dirty        bool // downloads to save
	lastActivity time.Time

	answers chan struct{} // search answers being sent, at most its capacity at once
	changed chan struct{}
	stop    chan struct{}
	stopped bool
}

// New makes a client for cfg, keeping its files in dir. Nothing connects before Start.
func New(cfg Config, dir string) *Client {
	c := &Client{
		dir:      dir,
		cfg:      cfg,
		state:    StateSetup,
		pierce:   map[uint32]*pending{},
		addrs:    map[string][]chan peerAddr{},
		peers:    map[string]*conn{},
		dialing:  map[string]chan struct{}{},
		upTokens: map[uint32]*Transfer{},
		folders:  map[uint32]chan map[string][]File{},
		token:    rand.Uint32() >> 1,
		answers:  make(chan struct{}, 6),
		changed:  make(chan struct{}),
		stop:     make(chan struct{}),
		bucket:   newBucket(),
	}
	os.MkdirAll(dir, 0o755)
	c.loadDownloads()
	return c
}

// Changed is closed at the next change of anything Status, the searches or the transfers show.
func (c *Client) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// notify wakes Changed's waiters. Called with c.mu held.
func (c *Client) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Client) nextToken() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token++
	return c.token
}

// Config is the configuration in use.
func (c *Client) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Status is what the Q2's page and the phone show at the top.
type Status struct {
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
	Username     string `json:"username"`
	SharedFiles  int    `json:"shared_files"`
	SharedDirs   int    `json:"shared_dirs"`
	Scanning     bool   `json:"scanning"`
	Sharing      string `json:"sharing"`
	ShareActive  bool   `json:"share_active"`
	External     bool   `json:"external_power"`
	Battery      int    `json:"battery"`
	Port         int    `json:"port"`
	Downloading  int    `json:"downloading"`
	Queued       int    `json:"queued"`
	Finished     int    `json:"finished"`
	Failed       int    `json:"failed"`
	Uploading    int    `json:"uploading"`
	UploadQueue  int    `json:"upload_queue"`
	HasParent    bool   `json:"distributed"`
	DownloadRate int64  `json:"download_rate"`
	UploadRate   int64  `json:"upload_rate"`
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{State: c.state, Error: c.err, Username: c.cfg.Username, Scanning: c.scanning,
		Sharing: c.cfg.Sharing, ShareActive: c.shareActive, External: c.pow.External, Battery: c.pow.Capacity,
		Port: c.port, HasParent: c.parent != nil}
	if c.shares != nil {
		s.SharedFiles, s.SharedDirs = c.shares.Files(), c.shares.Dirs()
	}
	for _, d := range c.downs {
		switch d.State {
		case Downloading, Starting:
			s.Downloading++
			s.DownloadRate += int64(d.Speed)
		case Finished:
			s.Finished++
		case Failed:
			s.Failed++
		case Paused:
		default:
			s.Queued++
		}
	}
	for _, u := range c.ups {
		switch u.State {
		case Uploading, Starting, Requesting:
			s.Uploading++
			s.UploadRate += int64(u.Speed)
		case Queued:
			s.UploadQueue++
		}
	}
	return s
}

// Busy reports whether a transfer is under way, so the Q2 should stay awake.
func (c *Client) Busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		if d.State == Downloading || d.State == Starting {
			return true
		}
	}
	for _, u := range c.ups {
		if u.State == Uploading || u.State == Starting {
			return true
		}
	}
	return false
}

// Pending reports whether a download is still to finish.
func (c *Client) Pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		if d.State != Finished && d.State != Failed && d.State != Paused {
			return true
		}
	}
	return false
}

// Start listens for peers and logs in, and runs the transfer and sharing loops until Stop.
func (c *Client) Start() {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	c.listen(cfg.Port)
	go c.serverLoop()
	go c.transferLoop()
	go c.powerLoop()
	c.Rescan()
}

// Stop disconnects everything; downloads are saved to resume next time.
func (c *Client) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	close(c.stop)
	c.state = StateStopped
	if c.server != nil {
		c.server.close()
	}
	if c.listener != nil {
		c.listener.Close()
	}
	for _, p := range c.peers {
		p.close()
	}
	if c.parent != nil {
		c.parent.close()
	}
	for _, d := range c.downs {
		if d.conn != nil {
			d.conn.Close()
		}
	}
	for _, u := range c.ups {
		if u.conn != nil {
			u.conn.Close()
		}
	}
	c.dirty = true
	c.mu.Unlock()
	c.saveDownloads()
}

// SetConfig takes a new configuration: a new account or server logs in again, a new port listens
// there, new shared folders are scanned.
func (c *Client) SetConfig(cfg Config) {
	c.mu.Lock()
	old := c.cfg
	c.cfg = cfg
	relogin := old.Username != cfg.Username || old.Password != cfg.Password || old.Server != cfg.Server
	if relogin {
		c.err = ""
		c.retries = 0
		c.reconnAt = time.Time{}
		if c.server != nil {
			c.server.close()
		} else if c.state != StateStopped {
			c.state = StateConnecting
		}
	}
	c.notify()
	c.mu.Unlock()
	if old.Port != cfg.Port {
		c.listen(cfg.Port)
		c.serverSend(coded(sSetWaitPort).u32(uint32(c.port)))
	}
	if strings.Join(old.Shares, "\n") != strings.Join(cfg.Shares, "\n") || old.Sharing != cfg.Sharing ||
		old.DownloadDir != cfg.DownloadDir {
		c.Rescan()
	}
	c.updateSharing()
}

// Reconnect logs in again now, after a refusal or another login took the account.
func (c *Client) Reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retries = 0
	c.reconnAt = time.Time{}
	c.err = ""
	if c.state == StateRefused || c.state == StateElsewhere {
		c.state = StateConnecting
	}
	c.notify()
}

func (c *Client) listen(port int) {
	c.mu.Lock()
	if c.listener != nil {
		c.listener.Close()
		c.listener = nil
	}
	c.mu.Unlock()
	for p := port; p < port+10; p++ {
		l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.listener, c.port = l, p
		c.mu.Unlock()
		go c.acceptLoop(l)
		return
	}
	log.Printf("no port to listen on from %d", port)
}

func (c *Client) acceptLoop(l net.Listener) {
	for {
		nc, err := l.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		go c.incoming(nc)
	}
}

// serverLoop keeps one logged-in server connection, waiting longer after each failure (up to five
// minutes) and not at all after a refused login or another login taking the account.
func (c *Client) serverLoop() {
	for {
		select {
		case <-c.stop:
			return
		case <-time.After(time.Second):
		}
		c.mu.Lock()
		cfg := c.cfg
		wait := c.state == StateRefused || c.state == StateElsewhere || c.state == StateStopped ||
			cfg.Username == "" || cfg.Password == "" || time.Now().Before(c.reconnAt) || c.server != nil
		if cfg.Username == "" && c.state != StateSetup {
			c.state = StateSetup
			c.notify()
		}
		c.mu.Unlock()
		if wait {
			continue
		}
		c.login(cfg)
	}
}

func (c *Client) setState(state, err string) {
	c.mu.Lock()
	c.state, c.err = state, err
	c.notify()
	c.mu.Unlock()
}

func (c *Client) backoff(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retries++
	d := time.Duration(5<<min(c.retries, 6)) * time.Second // 10 s doubling to 5 min 20 s
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	c.reconnAt = time.Now().Add(d)
	if c.state != StateStopped {
		c.state, c.err = StateOffline, reason
	}
	c.notify()
}

func (c *Client) login(cfg Config) {
	c.setState(StateConnecting, "")
	nc, err := net.DialTimeout("tcp4", cfg.Server, 20*time.Second)
	if err != nil {
		c.backoff("Can't reach the Soulseek server")
		return
	}
	sum := md5.Sum([]byte(cfg.Username + cfg.Password))
	s := newConn(c, nc, 'S', "")
	s.write(coded(sLogin).str(cfg.Username).str(cfg.Password).u32(MajorVersion).
		str(hex.EncodeToString(sum[:])).u32(MinorVersion).frame())
	nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := readFrame(nc, maxServerMessage)
	if err != nil || len(body) < 5 || binary.LittleEndian.Uint32(body) != sLogin {
		nc.Close()
		c.backoff("The Soulseek server didn't answer")
		return
	}
	r := &reader{b: body[4:]}
	if !r.boolean() {
		reason := r.str()
		detail := r.str()
		nc.Close()
		c.mu.Lock()
		c.state, c.err = StateRefused, refusal(reason, detail)
		c.notify()
		c.mu.Unlock()
		log.Printf("login refused: %s %s", reason, detail)
		return
	}
	r.str() // greeting
	ip := r.u32()
	nc.SetReadDeadline(time.Time{})
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(60 * time.Second)
	}
	c.mu.Lock()
	c.server, c.ourIP, c.state, c.err, c.retries = s, ip, StateOnline, "", 0
	port := c.port
	c.notify()
	c.mu.Unlock()
	log.Printf("logged in as %s", cfg.Username)
	s.write(coded(sSetWaitPort).u32(uint32(port)).frame())
	s.write(coded(sSetStatus).u32(2).frame())
	c.reportShares()
	c.distributedAnnounce()
	c.resumeTransfers()
	for {
		body, err := readFrame(nc, maxServerMessage)
		if err != nil {
			break
		}
		if len(body) >= 4 {
			c.serverMessage(binary.LittleEndian.Uint32(body), &reader{b: body[4:]})
		}
	}
	nc.Close()
	c.mu.Lock()
	if c.server == s {
		c.server = nil
	}
	if c.parent != nil {
		c.parent.close()
	}
	elsewhere := c.state == StateElsewhere
	c.mu.Unlock()
	if !elsewhere {
		c.backoff("Disconnected from the Soulseek server")
	}
}

func refusal(reason, detail string) string {
	switch reason {
	case "INVALIDPASS":
		return "Wrong password for this username"
	case "INVALIDUSERNAME":
		if detail != "" {
			return "Username not allowed: " + strings.TrimSuffix(detail, ".")
		}
		return "Username not allowed"
	case "EMPTYPASSWORD":
		return "The password is empty"
	case "INVALIDVERSION":
		return "The server refused this client's version"
	case "SVRFULL":
		return "The server is full; try again later"
	case "SVRPRIVATE":
		return "The server isn't taking new accounts"
	}
	if reason == "" {
		return "The server refused the login"
	}
	return "The server refused the login: " + reason
}

// serverSend writes to the server connection, if there is one.
func (c *Client) serverSend(m *msg) {
	c.mu.Lock()
	s := c.server
	c.mu.Unlock()
	if s != nil {
		s.write(m.frame())
	}
}

func (c *Client) serverMessage(code uint32, r *reader) {
	switch code {
	case sGetPeerAddress:
		user, ip, port := r.str(), r.u32(), r.u32()
		if r.err != nil {
			return
		}
		c.mu.Lock()
		waiters := c.addrs[user]
		delete(c.addrs, user)
		c.mu.Unlock()
		for _, w := range waiters {
			w <- peerAddr{ip, port}
		}
	case sConnectToPeer:
		user, typ, ip, port, token := r.str(), r.str(), r.u32(), r.u32(), r.u32()
		if r.err == nil && len(typ) == 1 {
			go c.connectBack(user, typ[0], ip, port, token)
		}
	case sMessageUser:
		id := r.u32()
		r.u32()
		user, text := r.str(), r.str()
		if r.err == nil {
			c.serverSend(coded(sMessageAcked).u32(id))
			log.Printf("message from %s: %q", Text(user), Text(text))
		}
	case sFileSearch: // a user or room search sent to us directly
		user, token, query := r.str(), r.u32(), r.str()
		if r.err == nil {
			c.answer(user, token, query)
		}
	case sRelogged:
		c.mu.Lock()
		c.state, c.err = StateElsewhere, "This account logged in somewhere else"
		c.notify()
		c.mu.Unlock()
	case sEmbeddedMessage:
		if r.u8() == dSearch {
			c.distribSearch(nil, r)
		}
	case sPossibleParents:
		n := int(r.u32())
		var list []peerCandidate
		for i := 0; i < n && i < 10 && r.err == nil; i++ {
			list = append(list, peerCandidate{r.str(), r.u32(), r.u32()})
		}
		if r.err == nil {
			go c.findParent(list)
		}
	case sResetDistributed:
		c.mu.Lock()
		p := c.parent
		c.mu.Unlock()
		if p != nil {
			p.close()
		}
	case sExcludedPhrases:
		n := int(r.u32())
		var phrases []string
		for i := 0; i < n && i < 10000 && r.err == nil; i++ {
			phrases = append(phrases, strings.ToLower(r.str()))
		}
		if r.err == nil {
			c.mu.Lock()
			c.excluded = phrases
			c.mu.Unlock()
		}
	case sCantConnectToPeer:
		token := r.u32()
		c.mu.Lock()
		p := c.pierce[token]
		c.mu.Unlock()
		if p != nil {
			p.fail()
		}
	}
}

// reportShares tells the server how many folders and files we share: none unless sharing is on.
func (c *Client) reportShares() {
	c.mu.Lock()
	dirs, files := 0, 0
	if c.shares != nil && c.cfg.Sharing != ShareOff {
		dirs, files = c.shares.Dirs(), c.shares.Files()
	}
	c.mu.Unlock()
	c.serverSend(coded(sSharedFoldersFile).u32(uint32(dirs)).u32(uint32(files)))
}

// Rescan indexes the shared folders again, on a goroutine.
func (c *Client) Rescan() {
	c.mu.Lock()
	if c.scanning {
		c.mu.Unlock()
		return
	}
	cfg := c.cfg
	if cfg.Sharing == ShareOff {
		c.shares = nil
		c.notify()
		c.mu.Unlock()
		c.reportShares()
		return
	}
	c.scanning = true
	c.notify()
	c.mu.Unlock()
	go func() {
		var roots []string
		for _, s := range cfg.Shares {
			if st, err := os.Stat(s); err == nil && st.IsDir() {
				roots = append(roots, s)
			}
		}
		shares := ScanShares(roots, filepath.Join(c.dir, "shares.cache"), filepath.Join(cfg.DownloadDir, incompleteDir))
		c.mu.Lock()
		c.shares, c.scanning = shares, false
		c.notify()
		c.mu.Unlock()
		log.Printf("sharing %d files in %d folders", shares.Files(), shares.Dirs())
		c.reportShares()
		c.updateSharing()
	}()
}

// Touch marks the phone or the Q2's page in use.
func (c *Client) Touch() {
	c.mu.Lock()
	c.lastActivity = time.Now()
	c.mu.Unlock()
}

// Idle is how long since the phone or the Q2's page was last used.
func (c *Client) Idle() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastActivity.IsZero() {
		c.lastActivity = time.Now()
	}
	return time.Since(c.lastActivity)
}
