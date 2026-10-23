package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/power"
	"github.com/group-a-study/q2-soulseek/internal/q2"
	"github.com/group-a-study/q2-soulseek/internal/q2spot"
	"github.com/group-a-study/q2-soulseek/internal/slsk"
)

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(25 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writePower(t *testing.T, online string) {
	for p, v := range map[string]string{"usb/type": "USB", "usb/online": online, "battery/type": "Battery",
		"battery/status": "Discharging", "battery/capacity": "64"} {
		os.MkdirAll(filepath.Join(power.Root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(power.Root, p), []byte(v+"\n"), 0o644)
	}
}

// world is the fake Soulseek server, bob sharing an album, and the add-on on a card of its own.
type world struct {
	srv  *slsk.FakeServer
	bob  *slsk.Client
	a    *app
	card string
	base string
	http *http.Client
}

func newWorld(t *testing.T) *world {
	q2.Card = "/no-card"
	power.Root = t.TempDir()
	writePower(t, "0")
	srv, err := slsk.NewFakeServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	music := filepath.Join(tmp, "bob", "Music", "Stereolab", "Dots and Loops")
	os.MkdirAll(music, 0o755)
	for i, n := range []string{"01 Brakhage.flac", "02 Miss Modular.flac", "03 The Flower Called Nowhere.flac"} {
		os.WriteFile(filepath.Join(music, n), bytes.Repeat([]byte{byte(i)}, 300<<10+i), 0o644)
	}
	bobCfg := slsk.Config{Server: srv.Addr(), Username: "bob", Password: "pw", Port: freePort(t),
		Sharing: slsk.ShareBattery, BatteryMin: 30, Shares: []string{filepath.Join(tmp, "bob", "Music")},
		DownloadDir: filepath.Join(tmp, "bob", "dl")}
	bobCfg.Defaults(tmp)
	bob := slsk.New(bobCfg, filepath.Join(tmp, "bob"))
	bob.Start()
	card := filepath.Join(tmp, "card")
	for _, d := range []string{"Music", "Photos", ".spotify"} {
		os.MkdirAll(filepath.Join(card, d), 0o755)
	}
	dir := filepath.Join(card, ".soulseek")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(fmt.Sprintf(`{"server":%q,"port":%d}`, srv.Addr(), freePort(t))), 0o644)
	a, err := start(options{dir: dir, card: card, statusFile: filepath.Join(tmp, "state"),
		browse: filepath.Join(tmp, "browse.sock"), control: filepath.Join(tmp, "ctl.sock"), httpPort: freePort(t)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.run(); close(done) }()
	t.Cleanup(func() {
		a.shutdown()
		<-done
		bob.Stop()
		srv.Close()
	})
	jar, _ := cookiejar.New(nil)
	return &world{srv: srv, bob: bob, a: a, card: card, base: fmt.Sprintf("http://127.0.0.1:%d", a.port),
		http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (w *world) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", w.base+"/api/"+path, bytes.NewReader(b))
	req.Header.Set("X-Q2", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (w *world) get(t *testing.T, path string, out any) int {
	t.Helper()
	resp, err := w.http.Get(w.base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// ask sends one Q2SPOT/1 request, as patch/soulseek.c does, and splits the reply.
func (w *world) ask(t *testing.T, op string, args ...string) (head []string, rows [][]string) {
	t.Helper()
	c, err := net.Dial("unix", w.a.opts.browse)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	line := "1\t" + op
	for _, a := range args {
		line += "\t" + q2spot.Escape(a)
	}
	c.Write([]byte("Q2SPOT/1\n" + line + "\n"))
	c.(*net.UnixConn).CloseWrite()
	b, _ := io.ReadAll(c)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) < 2 || lines[0] != "Q2SPOT/1" {
		t.Fatalf("reply %q", b)
	}
	head = strings.Split(lines[1], "\t")
	for _, l := range lines[2:] {
		f := strings.Split(l, "\t")
		for i := range f {
			f[i], _ = q2spot.Unescape(f[i])
		}
		rows = append(rows, f)
	}
	return head, rows
}

func titles(rows [][]string) string {
	var out []string
	for _, r := range rows {
		out = append(out, r[0]+":"+r[2])
	}
	return strings.Join(out, " | ")
}

func readState(t *testing.T, w *world) map[string]string {
	b, _ := os.ReadFile(w.a.state.Path)
	kv := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func TestAddOn(t *testing.T) {
	w := newWorld(t)
	set := w.a.settings()
	// A first start: a key and PIN made, the card's folders shared but the private ones, sharing off.
	if len(set.WebKey) != 12 || len(set.PIN) != 6 || set.Sharing != slsk.ShareOff || set.IdleMinutes != 30 {
		t.Fatalf("settings %+v", set)
	}
	if got := strings.Join(set.Shares, ","); got != filepath.Join(w.card, "Music")+","+filepath.Join(w.card, "Soulseek") {
		t.Fatalf("default shares %s", got)
	}
	st := readState(t, w)
	if st["state"] != "setup" || st["pin"] != set.PIN || st["hold"] != "0" {
		t.Fatalf("state file %v", st)
	}
	if st["addr"] != "" && (st["qr"] == "" || st["qrsize"] == "0") {
		t.Fatalf("an address but no QR code: %v", st)
	}

	// The page itself is open; its API isn't, until the PIN or the QR code's key.
	if code := w.get(t, "/", nil); code != 200 {
		t.Fatalf("index %d", code)
	}
	if code := w.get(t, "/api/state", nil); code != 401 {
		t.Fatalf("state unauthenticated: %d", code)
	}
	if code, _ := w.post(t, "login", map[string]string{"pin": "000000x"}); code != 403 {
		t.Fatalf("wrong PIN: %d", code)
	}
	if code := w.get(t, "/?k="+set.WebKey, nil); code != 302 {
		t.Fatalf("QR key: %d", code)
	}
	var snap snapshot
	if code := w.get(t, "/api/state", &snap); code != 200 || snap.Status.State != "setup" || snap.PIN != set.PIN {
		t.Fatalf("state %d %+v", code, snap.Status)
	}
	// Without X-Q2 a POST is refused, so another site can't post a form at it.
	resp, _ := w.http.Post(w.base+"/api/search", "application/json", strings.NewReader(`{"query":"x"}`))
	if resp.StatusCode != 400 {
		t.Fatalf("POST without X-Q2: %d", resp.StatusCode)
	}

	// The Q2's page before setup: the phone, or stop.
	if _, rows := w.ask(t, "menu"); titles(rows) != "remote:Set up on your phone | action:Stop Soulseek" {
		t.Fatalf("setup menu %s", titles(rows))
	}
	if code, out := w.post(t, "account", map[string]string{"username": " ", "password": "x"}); code != 400 {
		t.Fatalf("blank user %d %v", code, out)
	}
	if code, out := w.post(t, "account", map[string]string{"username": "alice", "password": "secret"}); code != 200 {
		t.Fatalf("account %d %v", code, out)
	}
	waitFor(t, "online", 10*time.Second, func() bool { return w.a.client.Status().State == slsk.StateOnline })
	waitFor(t, "bob sharing", 10*time.Second, func() bool { return w.bob.Status().ShareActive })

	// The long poll answers at once with a new version, and waits when nothing changed.
	var s1 snapshot
	w.get(t, "/api/state", &s1)
	began := time.Now()
	var s2 snapshot
	go func() { time.Sleep(300 * time.Millisecond); w.a.client.Touch(); w.a.client.Clear("finished") }()
	w.get(t, fmt.Sprintf("/api/state?since=%d", s1.V), &s2)
	if s2.V == s1.V || time.Since(began) < 200*time.Millisecond {
		t.Fatalf("long poll %d -> %d after %v", s1.V, s2.V, time.Since(began))
	}

	// Search from the phone; download the folder of a result.
	_, out := w.post(t, "search", map[string]string{"query": "stereolab modular"})
	id := int(out["id"].(float64))
	var res struct {
		Results []slsk.Result `json:"results"`
	}
	waitFor(t, "results", 10*time.Second, func() bool {
		w.get(t, fmt.Sprintf("/api/search?id=%d&from=0", id), &res)
		return len(res.Results) == 1
	})
	if r := res.Results[0]; r.User != "bob" || r.Name != "02 Miss Modular.flac" || r.Folder != `Music\Stereolab\Dots and Loops` {
		t.Fatalf("result %+v", r)
	}
	if code, _ := w.post(t, "download/folder", map[string]int{"search": id, "n": 0}); code != 200 {
		t.Fatalf("folder %d", code)
	}
	waitFor(t, "the album", 20*time.Second, func() bool {
		n := 0
		for _, d := range w.a.client.Downloads() {
			if d.State == slsk.Finished {
				n++
			}
		}
		return n == 3
	})
	got, err := os.ReadFile(filepath.Join(w.card, "Soulseek", "Dots and Loops", "03 The Flower Called Nowhere.flac"))
	if err != nil || len(got) != 300<<10+2 {
		t.Fatalf("downloaded %d %v", len(got), err)
	}

	// The Q2's page: the menu, a search's audio files (download on press, the folder on hold),
	// the downloads and a transfer's actions.
	head, rows := w.ask(t, "menu")
	if head[1] != "ok" || head[2] != "refresh" || len(rows) != 7 || rows[0][0] != "remote" || rows[1][0] != "search" ||
		rows[2][2] != "Downloads" || rows[2][3] != "3 finished" || rows[4][2] != "Sharing: Off" {
		t.Fatalf("menu %q %s", head, titles(rows))
	}
	head, rows = w.ask(t, "search", "dots loops brakhage")
	if head[1] != "ok" {
		t.Fatalf("search %q", head)
	}
	waitFor(t, "a Q2 result", 10*time.Second, func() bool {
		_, rows = w.ask(t, "search", "dots loops brakhage")
		return len(rows) == 1 && rows[0][0] == "action"
	})
	if r := rows[0]; r[1] != "download" || r[5] != "downloadfolder" || r[2] != "01 Brakhage.flac" ||
		!strings.HasPrefix(r[3], "bob · FLAC · 300 KB") {
		t.Fatalf("Q2 result %q", r)
	}
	if head, _ = w.ask(t, "download", rows[0][4]); head[1] != "ok" || head[2] != "Downloading 01 Brakhage.flac" {
		t.Fatalf("download %q", head)
	}
	if head, _ = w.ask(t, "download", "99.0"); head[1] != "unavailable" {
		t.Fatalf("a gone result %q", head)
	}
	_, rows = w.ask(t, "downloads")
	if len(rows) < 3 || rows[0][0] != "list" || rows[0][1] != "transfer" {
		t.Fatalf("downloads %s", titles(rows))
	}
	_, acts := w.ask(t, "transfer", rows[len(rows)-1][4])
	if titles(acts[2:]) != "action:Remove from List" {
		t.Fatalf("finished transfer %s", titles(acts))
	}
	// Sharing steps Off, While charging, Charging or battery, Off; the setting is saved.
	if head, _ = w.ask(t, "set", "sharing"); head[2] != "Sharing: While charging" || w.a.settings().Sharing != slsk.ShareCharging {
		t.Fatalf("set %q", head)
	}
	b, _ := os.ReadFile(filepath.Join(w.a.dir, "config.json"))
	if !strings.Contains(string(b), `"sharing": "charging"`) {
		t.Fatal("not saved")
	}
	// On battery a charging-only share is paused; the state file says so, and the Q2 isn't held
	// for it (the phone was just used, so it is held for that).
	waitFor(t, "the state file", 3*time.Second, func() bool { return readState(t, w)["sharing"] == "Paused" })
	writePower(t, "1")
	waitFor(t, "sharing on the cable", 15*time.Second, func() bool { return readState(t, w)["sharing"] == "On" })
	if st := readState(t, w); st["hold"] != "1" || st["note"] != "Online as alice" || st["done"] != "4" && st["done"] != "3" {
		t.Fatalf("state %v", st)
	}

	// Stop from the Q2: the state file goes, as the payload looks for.
	w.ask(t, "stop")
	waitFor(t, "the stop", 5*time.Second, func() bool { _, err := os.Stat(w.a.state.Path); return os.IsNotExist(err) })
}

// TestPreview serves the phone page against the fake network for a look in a browser:
// Q2_PREVIEW=1 go test -run Preview ./cmd/q2-soulseek prints the address and waits.
func TestPreview(t *testing.T) {
	if os.Getenv("Q2_PREVIEW") == "" {
		t.Skip("set Q2_PREVIEW=1")
	}
	w := newWorld(t)
	w.a.update(func(s *settings) { s.Username, s.Password, s.Sharing = "alice", "pw", slsk.ShareCharging })
	c := w.a.client
	waitFor(t, "online", 10*time.Second, func() bool { return c.Status().State == slsk.StateOnline })
	waitFor(t, "bob sharing", 10*time.Second, func() bool { return w.bob.Status().ShareActive })
	id, _ := c.Search("stereolab")
	waitFor(t, "results", 10*time.Second, func() bool { _, r, _ := c.Results(id, 0); return len(r) == 3 })
	r, _ := c.Result(id, 1)
	r.Download(c)
	c.Download("carol", `Music\Broadcast\Tender Buttons\01 I Found The F.mp3`, 6<<20)
	c.Download("bob", `Music\Stereolab\Emperor Tomato Ketchup\03 Cybele's Reverie.flac`, 31<<20)
	c.Search("boards of canada geogaddi")
	fmt.Printf("PREVIEW %s/?k=%s\n", w.base, w.a.settings().WebKey)
	select {}
}
