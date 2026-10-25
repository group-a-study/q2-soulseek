package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/slsk"
)

//go:embed web/index.html
var indexHTML []byte

const cookieName = "q2slsk"

// web is the phone's page: index.html and a JSON API behind the key the QR code carries (or the
// PIN on the Q2's screen), kept in a cookie.
type web struct {
	a *app

	mu      sync.Mutex
	ver     int
	wake    chan struct{}
	fails   int
	lockout time.Time
}

func serveWeb(a *app, l net.Listener) {
	w := &web{a: a, wake: make(chan struct{})}
	go w.follow()
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/api/login", w.login)
	mux.HandleFunc("/api/", w.api)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	srv.Serve(l)
}

// follow counts the client's changes, at most two a second, waking the long polls. The next change's
// channel is taken before the pause, so a change during it is counted at the end of it.
func (w *web) follow() {
	ch := w.a.client.Changed()
	for {
		<-ch
		ch = w.a.client.Changed()
		time.Sleep(500 * time.Millisecond)
		w.bump()
	}
}

func (w *web) bump() {
	w.mu.Lock()
	w.ver++
	close(w.wake)
	w.wake = make(chan struct{})
	w.mu.Unlock()
}

func (w *web) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	key := w.a.settings().WebKey
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(key)) == 1
}

func (w *web) setCookie(rw http.ResponseWriter) {
	http.SetCookie(rw, &http.Cookie{Name: cookieName, Value: w.a.settings().WebKey, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
}

func (w *web) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(w.a.settings().WebKey)) == 1 {
			w.setCookie(rw)
		}
		http.Redirect(rw, r, "/", http.StatusFound)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Write(indexHTML)
}

func reply(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

func fail(rw http.ResponseWriter, code int, msg string) {
	reply(rw, code, map[string]string{"error": msg})
}

// login takes the Q2's PIN; ten wrong ones lock it for a minute.
func (w *web) login(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		fail(rw, http.StatusBadRequest, "bad request")
		return
	}
	var body struct{ PIN string }
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1024)).Decode(&body)
	w.mu.Lock()
	locked := time.Now().Before(w.lockout)
	w.mu.Unlock()
	if locked {
		fail(rw, http.StatusTooManyRequests, "Too many tries. Wait a minute.")
		return
	}
	pin := strings.TrimSpace(body.PIN)
	if subtle.ConstantTimeCompare([]byte(pin), []byte(w.a.settings().PIN)) != 1 {
		w.mu.Lock()
		if w.fails++; w.fails >= 10 {
			w.fails, w.lockout = 0, time.Now().Add(time.Minute)
		}
		w.mu.Unlock()
		fail(rw, http.StatusForbidden, "That's not the PIN on the Q2's screen.")
		return
	}
	w.mu.Lock()
	w.fails = 0
	w.mu.Unlock()
	w.setCookie(rw)
	reply(rw, http.StatusOK, map[string]bool{"ok": true})
}

func (w *web) api(rw http.ResponseWriter, r *http.Request) {
	if !w.authed(r) {
		fail(rw, http.StatusUnauthorized, "sign in")
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("X-Q2") != "1" {
		fail(rw, http.StatusBadRequest, "bad request")
		return
	}
	w.a.mu.Lock()
	w.a.webAt = time.Now()
	w.a.mu.Unlock()
	w.a.client.Touch()
	route := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.Method == http.MethodGet {
		switch route {
		case "state":
			w.state(rw, r)
		case "search":
			w.results(rw, r)
		default:
			http.NotFound(rw, r)
		}
		return
	}
	if r.Method != http.MethodPost {
		fail(rw, http.StatusMethodNotAllowed, "method")
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&body); err != nil && err != io.EOF {
		fail(rw, http.StatusBadRequest, "bad body")
		return
	}
	str := func(k string) string {
		var s string
		json.Unmarshal(body[k], &s)
		return s
	}
	num := func(k string) int {
		var n int
		json.Unmarshal(body[k], &n)
		return n
	}
	c := w.a.client
	switch route {
	case "search":
		id, err := c.Search(str("query"))
		if err != nil {
			fail(rw, http.StatusConflict, err.Error())
			return
		}
		reply(rw, http.StatusOK, map[string]uint32{"id": id})
	case "search/close":
		c.CloseSearch(uint32(num("id")))
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case "download":
		var ns []int
		json.Unmarshal(body["n"], &ns)
		got := 0
		for _, n := range ns {
			if res, ok := c.Result(uint32(num("search")), n); ok {
				res.Download(c)
				got++
			}
		}
		reply(rw, http.StatusOK, map[string]int{"queued": got})
	case "download/folder":
		res, ok := c.Result(uint32(num("search")), num("n"))
		if !ok {
			fail(rw, http.StatusNotFound, "That result is gone; search again.")
			return
		}
		w.downloadFolder(res)
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case "transfer":
		if err := c.Action(num("id"), str("action")); err != nil {
			fail(rw, http.StatusConflict, err.Error())
			return
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case "clear":
		c.Clear(str("which"))
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case "account":
		user, pass := strings.TrimSpace(str("username")), str("password")
		if err := validUser(user); err != nil {
			fail(rw, http.StatusBadRequest, err.Error())
			return
		}
		if pass == "" {
			pass = w.a.settings().Password
		}
		if pass == "" {
			fail(rw, http.StatusBadRequest, "Choose a password.")
			return
		}
		w.save(rw, func(s *settings) { s.Username, s.Password = user, pass })
	case "settings":
		w.settings(rw, body)
	case "reconnect":
		c.Reconnect()
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case "rescan":
		c.Rescan()
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case "access/reset":
		w.save(rw, func(s *settings) { s.WebKey, s.PIN = newKey(), newPIN() })
		w.setCookie(rw)
	case "stop":
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); w.a.shutdown() }()
	default:
		http.NotFound(rw, r)
	}
}

func (w *web) save(rw http.ResponseWriter, fn func(*settings)) {
	if err := w.a.update(fn); err != nil {
		fail(rw, http.StatusInternalServerError, "Couldn't save to the card: "+err.Error())
		return
	}
	w.bump()
	reply(rw, http.StatusOK, map[string]bool{"ok": true})
}

// validUser is the server's rule: 1-30 printable ASCII characters, no spaces at the ends.
func validUser(u string) error {
	if u == "" {
		return errors.New("Choose a username.")
	}
	if len(u) > 30 {
		return errors.New("A username is at most 30 characters.")
	}
	for _, r := range u {
		if r < 32 || r > 126 {
			return errors.New("Use only letters, digits and plain punctuation.")
		}
	}
	return nil
}

func (w *web) settings(rw http.ResponseWriter, body map[string]json.RawMessage) {
	var in struct {
		Sharing     *string   `json:"sharing"`
		BatteryMin  *int      `json:"battery_min"`
		Shares      *[]string `json:"shares"`
		DownloadDir *string   `json:"download_dir"`
		Slots       *int      `json:"slots"`
		UploadKBps  *int      `json:"upload_kbps"`
		Port        *int      `json:"port"`
		IdleMinutes *int      `json:"idle_minutes"`
		MaxResults  *int      `json:"max_results"`
	}
	raw, _ := json.Marshal(body)
	json.Unmarshal(raw, &in)
	card := w.a.card
	inCard := func(p string) (string, bool) {
		p = filepath.Clean(filepath.Join(card, strings.TrimPrefix(p, card)))
		return p, p == card || strings.HasPrefix(p, card+"/")
	}
	if in.Shares != nil {
		for i, p := range *in.Shares {
			q, ok := inCard(p)
			if !ok {
				fail(rw, http.StatusBadRequest, "Shared folders must be on the card.")
				return
			}
			(*in.Shares)[i] = q
		}
	}
	if in.DownloadDir != nil {
		q, ok := inCard(*in.DownloadDir)
		if !ok || q == card {
			fail(rw, http.StatusBadRequest, "Choose a folder on the card for downloads.")
			return
		}
		*in.DownloadDir = q
	}
	w.save(rw, func(s *settings) {
		if in.Sharing != nil {
			s.Sharing = *in.Sharing
		}
		if in.BatteryMin != nil {
			s.BatteryMin = *in.BatteryMin
		}
		if in.Shares != nil {
			s.Shares, s.SharesSet = *in.Shares, true
		}
		if in.DownloadDir != nil {
			s.DownloadDir = *in.DownloadDir
		}
		if in.Slots != nil {
			s.Slots = *in.Slots
		}
		if in.UploadKBps != nil {
			s.UploadKBps = *in.UploadKBps
		}
		if in.Port != nil {
			s.Port = *in.Port
		}
		if in.IdleMinutes != nil && *in.IdleMinutes >= 0 {
			s.IdleMinutes = *in.IdleMinutes
		}
		if in.MaxResults != nil {
			s.MaxResults = *in.MaxResults
		}
	})
}

// downloadFolder queues a result's whole folder, as the user shares it now, or failing an answer,
// the files the search showed from it.
func (w *web) downloadFolder(res slsk.Result) {
	c := w.a.client
	user, folder := res.FolderOf()
	var known []slsk.File
	for _, s := range c.Searches() {
		_, list, _ := c.Results(s.Token, 0)
		for _, x := range list {
			if u, f := x.FolderOf(); u == user && f == folder {
				known = append(known, x.File())
			}
		}
	}
	c.DownloadFolder(user, folder, known)
}

type snapshot struct {
	V         int               `json:"v"`
	Version   string            `json:"version"`
	Status    slsk.Status       `json:"status"`
	Addr      string            `json:"addr"`
	PIN       string            `json:"pin"`
	Searches  []slsk.SearchInfo `json:"searches"`
	Downloads []slsk.Info       `json:"downloads"`
	Uploads   []slsk.Info       `json:"uploads"`
	Settings  map[string]any    `json:"settings"`
	Folders   []string          `json:"folders"`
	Card      string            `json:"card"`
}

// state is everything the page shows, once something changed since the version the page has
// (waiting up to 25 s), so the page follows along with one request at a time.
func (w *web) state(rw http.ResponseWriter, r *http.Request) {
	since, err := strconv.Atoi(r.URL.Query().Get("since"))
	w.mu.Lock()
	ver, wake := w.ver, w.wake
	w.mu.Unlock()
	if err == nil && since == ver {
		select {
		case <-wake:
		case <-time.After(25 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.mu.Lock()
		ver = w.ver
		w.mu.Unlock()
	}
	a := w.a
	c := a.client
	s := a.settings()
	snap := snapshot{V: ver, Version: version, Status: c.Status(), Addr: strings.TrimSuffix(a.URL(false), "/"),
		PIN: s.PIN, Searches: c.Searches(), Downloads: trim(c.Downloads(), 100), Uploads: trim(c.Uploads(), 50),
		Card: a.card}
	for _, f := range CardFolders(a.card) {
		snap.Folders = append(snap.Folders, strings.TrimPrefix(f, a.card+"/"))
	}
	rel := func(p string) string { return strings.TrimPrefix(strings.TrimPrefix(p, a.card), "/") }
	shares := []string{}
	for _, p := range s.Shares {
		shares = append(shares, rel(p))
	}
	snap.Settings = map[string]any{"username": s.Username, "has_password": s.Password != "", "sharing": s.Sharing,
		"battery_min": s.BatteryMin, "shares": shares, "download_dir": rel(s.DownloadDir), "slots": s.Slots,
		"upload_kbps": s.UploadKBps, "port": s.Port, "idle_minutes": s.IdleMinutes, "max_results": s.MaxResults}
	reply(rw, http.StatusOK, snap)
}

// trim keeps every transfer still under way and the newest ended ones, up to keep of those.
func trim(l []slsk.Info, keep int) []slsk.Info {
	out := l[:0:0]
	ended := 0
	for _, t := range l {
		if t.State == slsk.Finished {
			if ended++; ended > keep {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// resultsPage is how many results one reply carries; the page asks again for the rest, so the Q2
// never encodes a search's thousands at once.
const resultsPage = 500

func (w *web) results(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	info, list, ok := w.a.client.Results(uint32(id), from)
	if !ok {
		fail(rw, http.StatusNotFound, "That search is gone.")
		return
	}
	reply(rw, http.StatusOK, map[string]any{"search": info, "from": from, "results": list[:min(len(list), resultsPage)]})
}
