// q2-soulseek is a Q2 Pod card app (contrib/shanlingq2/README): a Soulseek client that runs from the
// card's .soulseek folder (copied to /tmp first, so the card is never busy) with a web page for a
// phone on the same Wi-Fi, Q2SPOT/1 for the Q2's own Soulseek page, and a status file the Q2 reads
// for its QR code and to stay awake while files move.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/q2"
	"github.com/group-a-study/q2-soulseek/internal/q2spot"
	"github.com/group-a-study/q2-soulseek/internal/qr"
	"github.com/group-a-study/q2-soulseek/internal/slsk"
)

const version = "q2-v1.1"

// settings is .soulseek/config.json: the client's configuration, the phone's key and PIN, and when
// to stop by itself.
type settings struct {
	slsk.Config
	WebKey      string `json:"web_key"`
	PIN         string `json:"pin"`
	IdleMinutes int    `json:"idle_minutes"` // 0: never
	SharesSet   bool   `json:"shares_set"`   // Shares was chosen, so an empty list means none
}

type app struct {
	dir    string
	card   string
	opts   options
	client *slsk.Client
	state  *q2.State
	web    net.Listener
	ctl    io.Closer

	mu        sync.Mutex
	set       settings
	ip        string
	port      int
	qrHex     string
	qrSize    int
	qrFor     string
	webAt     time.Time // the phone's last request
	busyAt    time.Time // a transfer last moved
	pendingAt time.Time // downloads started waiting

	quit chan struct{}
	once sync.Once
}

type options struct {
	dir, card, statusFile, browse, control string
	httpPort                               int
}

func main() {
	var o options
	flag.StringVar(&o.dir, "dir", "/mnt/mmc/.soulseek", "the add-on's folder on the card")
	flag.StringVar(&o.card, "card", "/mnt/mmc", "the card's mount point")
	flag.StringVar(&o.statusFile, "status-file", "/tmp/q2-soulseek.state", "the status file Q2 Pod reads")
	flag.StringVar(&o.browse, "browse-socket", "/tmp/q2-soulseek.browse.sock", "Q2SPOT/1 for the Q2's page")
	flag.StringVar(&o.control, "control-socket", "/tmp/q2-soulseek.sock", "datagrams: q quits")
	flag.IntVar(&o.httpPort, "http-port", 8080, "the phone page's port (the next free one if taken)")
	flag.Parse()
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(48 << 20)
	q2.Card = o.card
	q2.SetupLog("q2-soulseek")
	a, err := start(o)
	if err != nil {
		log.Fatal(err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; a.shutdown() }()
	a.run()
	os.Exit(0)
}

// start logs in and serves the phone page, the Q2's page and the control socket.
func start(o options) (*app, error) {
	os.MkdirAll(o.dir, 0o755)
	a := &app{dir: o.dir, card: o.card, opts: o, state: &q2.State{Path: o.statusFile}, quit: make(chan struct{})}
	a.set = loadSettings(o.dir, o.card)
	a.client = slsk.New(a.set.Config, o.dir)
	l, port := listenHTTP(o.httpPort)
	if l == nil {
		return nil, fmt.Errorf("no port for the web page from %d", o.httpPort)
	}
	a.client.Start()
	log.Printf("%s starting", version)
	a.port, a.web = port, l
	go serveWeb(a, l)
	go func() {
		if err := q2spot.Serve(o.browse, a.device); err != nil {
			log.Printf("browse socket: %v", err)
		}
	}()
	if c, err := q2.Control(o.control, func(cmd []byte) {
		if len(cmd) > 0 && cmd[0] == 'q' {
			a.shutdown()
		}
	}); err == nil {
		a.ctl = c
	}
	a.writeState()
	return a, nil
}

// run writes the status file every second and stops by itself when idle, until shutdown.
func (a *app) run() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			a.client.Stop()
			a.web.Close()
			if a.ctl != nil {
				a.ctl.Close()
			}
			a.state.Remove()
			os.Remove(a.opts.browse)
			os.Remove(a.opts.control)
			log.Printf("stopped")
			return
		case <-t.C:
			a.writeState()
			if a.idle() {
				log.Printf("idle: stopping")
				a.shutdown()
			}
		}
	}
}

func (a *app) shutdown() { a.once.Do(func() { close(a.quit) }) }

func newKey() string { return q2.Key(12) }
func newPIN() string { return q2.PIN(6) }

// Folders a card's owner keeps for themselves; not shared unless chosen.
var private = map[string]bool{"photos": true, "books": true, "videos": true, "eq": true, "radio": true,
	"q2pod": true, "system volume information": true, "lost.dir": true, "dcim": true, "android": true}

// CardFolders is the card's top-level folders, hidden ones left out.
func CardFolders(card string) []string {
	entries, _ := os.ReadDir(card)
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "$") {
			out = append(out, filepath.Join(card, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func loadSettings(dir, card string) settings {
	s := settings{IdleMinutes: 30} // what a file without the key means
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	s.Defaults(card)
	if s.IdleMinutes < 0 {
		s.IdleMinutes = 0
	}
	if !s.SharesSet {
		s.Shares = nil
		for _, f := range CardFolders(card) {
			if !private[strings.ToLower(filepath.Base(f))] {
				s.Shares = append(s.Shares, f)
			}
		}
		if !contains(s.Shares, s.DownloadDir) {
			s.Shares = append(s.Shares, s.DownloadDir)
		}
	}
	dirty := false
	if s.WebKey == "" {
		s.WebKey, dirty = newKey(), true
	}
	if s.PIN == "" {
		s.PIN, dirty = newPIN(), true
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		dirty = true
	}
	if dirty {
		saveSettings(dir, s)
	}
	return s
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if filepath.Clean(x) == filepath.Clean(s) {
			return true
		}
	}
	return false
}

func saveSettings(dir string, s settings) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return q2.WriteFile(filepath.Join(dir, "config.json"), b)
}

func (a *app) settings() settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.set
}

// update changes the settings with fn, saves them and hands the client its part.
func (a *app) update(fn func(*settings)) error {
	a.mu.Lock()
	s := a.set
	fn(&s)
	s.Defaults(a.card)
	err := saveSettings(a.dir, s)
	if err == nil {
		a.set = s
	}
	a.mu.Unlock()
	if err == nil {
		a.client.SetConfig(s.Config)
	}
	return err
}

func localIP() string { return q2.LocalIP() }

func listenHTTP(port int) (net.Listener, int) {
	for p := port; p < port+10; p++ {
		if l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p)); err == nil {
			return l, p
		}
	}
	return nil, 0
}

// URL is the page's address, and with the key the one the QR code opens (signed in at once).
func (a *app) URL(withKey bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ip == "" {
		return ""
	}
	u := fmt.Sprintf("http://%s:%d/", a.ip, a.port)
	if withKey {
		u += "?k=" + a.set.WebKey
	}
	return u
}

// hold is whether the Q2 should stay awake: a transfer moving, downloads waiting (for at most 30
// minutes without one moving while on battery), the phone in use, or sharing while on a cable.
func (a *app) hold(st slsk.Status) bool {
	now := time.Now()
	busy, pending := a.client.Busy(), a.client.Pending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if busy {
		a.busyAt = now
	}
	if !pending {
		a.pendingAt = time.Time{}
	} else if a.pendingAt.IsZero() {
		a.pendingAt = now
	}
	last := a.busyAt
	if a.pendingAt.After(last) {
		last = a.pendingAt
	}
	waiting := pending && (st.External || now.Sub(last) < 30*time.Minute)
	return busy || waiting || now.Sub(a.webAt) < 2*time.Minute || (st.ShareActive && st.External)
}

// idle is whether to stop by itself: nothing to hold the Q2 for, no downloads to finish, on battery,
// and neither the phone nor the Q2's page used for IdleMinutes.
func (a *app) idle() bool {
	s := a.settings()
	if s.IdleMinutes == 0 {
		return false
	}
	st := a.client.Status()
	return !a.hold(st) && !a.client.Pending() && !st.External &&
		a.client.Idle() > time.Duration(s.IdleMinutes)*time.Minute
}

// note is the line under the QR code.
func note(st slsk.Status, ip string) string {
	switch {
	case ip == "":
		return "Connect the Q2 to Wi-Fi"
	case st.State == slsk.StateSetup:
		return "Scan to choose a Soulseek name"
	case st.State == slsk.StateConnecting:
		return "Connecting to Soulseek…"
	case st.State == slsk.StateOnline:
		return "Online as " + st.Username
	case st.Error != "":
		return st.Error
	}
	return "Offline"
}

// writeState rewrites the status file (q2.State skips an unchanged one): what the Q2's page shows
// and whether it holds standby off.
func (a *app) writeState() {
	ip := localIP()
	a.mu.Lock()
	a.ip = ip
	a.mu.Unlock()
	st := a.client.Status()
	url := a.URL(true)
	a.mu.Lock()
	if url != a.qrFor {
		a.qrFor, a.qrHex, a.qrSize = url, "", 0
		if url != "" {
			if c, err := qr.Encode([]byte(url)); err == nil {
				a.qrHex, a.qrSize = c.Hex(), c.Size
			}
		}
	}
	qrHex, qrSize, pin := a.qrHex, a.qrSize, a.set.PIN
	a.mu.Unlock()
	sharing := "Off"
	if st.Sharing != slsk.ShareOff {
		sharing = "Paused"
		if st.ShareActive {
			sharing = "On"
		}
	}
	hold := "0"
	if a.hold(st) {
		hold = "1"
	}
	a.state.Write(map[string]string{
		"state":   st.State,
		"note":    note(st, ip),
		"addr":    strings.TrimSuffix(strings.TrimPrefix(a.URL(false), "http://"), "/"),
		"pin":     pin,
		"qr":      qrHex,
		"qrsize":  fmt.Sprint(qrSize),
		"hold":    hold,
		"sharing": sharing,
		"dl":      fmt.Sprint(st.Downloading),
		"queued":  fmt.Sprint(st.Queued),
		"done":    fmt.Sprint(st.Finished),
		"up":      fmt.Sprint(st.Uploading),
		"version": version,
	})
}
