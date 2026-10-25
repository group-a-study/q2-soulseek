package slsk

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/power"
	"github.com/group-a-study/q2-soulseek/internal/q2"
)

// TestMain sets the package-wide paths once: a stopped client's goroutines can outlive their test.
func TestMain(m *testing.M) {
	q2.Card = "/no-card" // the test's folders are not the card: no mount checks
	power.Root, _ = os.MkdirTemp("", "q2-soulseek-power")
	power.Gauge = filepath.Join(power.Root, "cw2015_capacity")
	code := m.Run()
	os.RemoveAll(power.Root)
	os.Exit(code)
}

func setPower(t *testing.T, online bool, capacity int) {
	t.Helper()
	write := func(p, v string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	on := "0"
	if online {
		on = "1"
	}
	write(filepath.Join(power.Root, "axp2101_charger/type"), "USB")
	write(filepath.Join(power.Root, "axp2101_charger/online"), on)
	write(filepath.Join(power.Root, "battery/type"), "Battery")
	write(filepath.Join(power.Root, "battery/status"), map[bool]string{true: "Charging", false: "Discharging"}[online])
	write(filepath.Join(power.Root, "battery/capacity"), itoa(capacity))
}

// flac is a FLAC file's head: fLaC and a STREAMINFO for 16-bit 44.1 kHz, 180 s long, then data.
func flac(n int, seed int64) []byte {
	b := []byte("fLaC")
	b = append(b, 0x80, 0, 0, 34) // the last metadata block, STREAMINFO, 34 bytes
	si := make([]byte, 34)
	rate, bits, total := uint64(44100), uint64(16), uint64(44100*180)
	v := rate<<44 | 1<<41 | (bits-1)<<36 | total // 20 rate, 3 channels-1, 5 bits-1, 36 samples
	binary.BigEndian.PutUint64(si[10:], v)
	b = append(b, si...)
	rnd := rand.New(rand.NewSource(seed))
	for len(b) < n {
		b = append(b, byte(rnd.Intn(256)))
	}
	return b
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
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

func download(c *Client, id int) Info {
	for _, d := range c.Downloads() {
		if d.ID == id {
			return d
		}
	}
	return Info{}
}

func TestSearchDownloadAndSharing(t *testing.T) {
	setPower(t, true, 60)
	srv, err := NewFakeServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	tmp := t.TempDir()
	music := filepath.Join(tmp, "bob", "Music")
	album := filepath.Join(music, "Boards of Canada", "Geogaddi (2002)")
	os.MkdirAll(album, 0o755)
	song := flac(3<<20+12345, 1) // past several 512 KB writes, not a whole number of them
	os.WriteFile(filepath.Join(album, "01 Ready Lets Go.flac"), song, 0o644)
	os.WriteFile(filepath.Join(album, "02 Music Is Math.flac"), flac(700<<10, 2), 0o644)
	os.WriteFile(filepath.Join(album, "cover.jpg"), []byte("jpeg"), 0o644)
	os.WriteFile(filepath.Join(music, ".hidden.flac"), []byte("x"), 0o644)

	bob := New(Config{Server: srv.Addr(), Username: "bob", Password: "pw", Port: freePort(t),
		Sharing: ShareCharging, Shares: []string{music}, DownloadDir: filepath.Join(tmp, "bob", "dl")},
		filepath.Join(tmp, "bob"))
	alice := New(Config{Server: srv.Addr(), Username: "alice", Password: "pw", Port: freePort(t),
		Sharing: ShareOff, DownloadDir: filepath.Join(tmp, "alice", "Soulseek")}, filepath.Join(tmp, "alice"))
	for _, c := range []*Client{bob, alice} {
		c.cfg.Defaults(tmp)
		c.Start()
		defer c.Stop()
	}
	waitFor(t, "both online", 5*time.Second, func() bool {
		return bob.Status().State == StateOnline && alice.Status().State == StateOnline
	})
	waitFor(t, "bob's shares", 5*time.Second, func() bool { st := bob.Status(); return st.SharedFiles == 3 && st.ShareActive })

	// Alice searches; bob answers with what matches every word, hidden files left out.
	tok, err := alice.Search("geogaddi math")
	if err != nil {
		t.Fatal(err)
	}
	var res []Result
	waitFor(t, "a result", 10*time.Second, func() bool { _, res, _ = alice.Results(tok, 0); return len(res) > 0 })
	if len(res) != 1 || res[0].Name != "02 Music Is Math.flac" || res[0].User != "bob" ||
		res[0].Folder != `Music\Boards of Canada\Geogaddi (2002)` {
		t.Fatalf("results %+v", res)
	}
	if r := res[0]; r.Duration != 180 || r.Rate != 44100 || r.Depth != 16 || !r.Free || r.Ext != "flac" {
		t.Fatalf("attributes %+v", r)
	}
	tok, _ = alice.Search("ready lets")
	waitFor(t, "the other result", 10*time.Second, func() bool { _, res, _ = alice.Results(tok, 0); return len(res) == 1 })

	// A part left by an earlier try resumes from its end.
	r := res[0]
	id := r.Download(alice)
	part := filepath.Join(tmp, "alice", "Soulseek", incompleteDir, safeName(itoa(id)+"-"+r.Name))
	os.MkdirAll(filepath.Dir(part), 0o755)
	os.WriteFile(part, song[:1<<20], 0o644)
	waitFor(t, "the download", 20*time.Second, func() bool { return download(alice, id).State == Finished })
	got, err := os.ReadFile(filepath.Join(tmp, "alice", "Soulseek", "Geogaddi (2002)", "01 Ready Lets Go.flac"))
	if err != nil || !bytes.Equal(got, song) {
		t.Fatalf("downloaded %d bytes, err %v", len(got), err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("the part file is left over")
	}
	waitFor(t, "bob's upload finished", 5*time.Second, func() bool {
		u := bob.Uploads()
		return len(u) == 1 && u[0].State == Finished
	})

	// On battery bob shares nothing: no answers, and a request waits in his queue.
	setPower(t, false, 60)
	bob.updateSharing()
	if bob.Status().ShareActive {
		t.Fatal("sharing on battery")
	}
	tok, _ = alice.Search("geogaddi")
	time.Sleep(500 * time.Millisecond)
	if _, res, _ := alice.Results(tok, 0); len(res) != 0 {
		t.Fatal("bob answered on battery")
	}
	id = alice.Download("bob", `Music\Boards of Canada\Geogaddi (2002)\02 Music Is Math.flac`, 700<<10)
	waitFor(t, "alice's place in bob's queue", 10*time.Second, func() bool { return download(alice, id).State == Remote })
	time.Sleep(3 * time.Second)
	if d := download(alice, id); d.State != Remote || d.Bytes != 0 {
		t.Fatalf("uploaded on battery: %+v", d)
	}
	// The cable back: the upload starts on its own.
	setPower(t, true, 60)
	bob.updateSharing()
	waitFor(t, "the queued download", 20*time.Second, func() bool { return download(alice, id).State == Finished })

	// A file bob doesn't share is refused.
	id = alice.Download("bob", `Music\secret.flac`, 10)
	waitFor(t, "the refusal", 10*time.Second, func() bool { return download(alice, id).State == Failed })
	if e := download(alice, id).Error; e != "The user doesn't share this file" {
		t.Fatalf("refusal %q", e)
	}

	// With bob's address wrong, the connection is made the other way round (bob pierces to alice).
	srv.BadAddress("bob")
	bob.mu.Lock()
	for _, p := range bob.peers {
		p.close()
	}
	bob.mu.Unlock()
	alice.mu.Lock()
	for _, p := range alice.peers {
		p.close()
	}
	alice.mu.Unlock()
	alice.Action(download(alice, id).ID, "remove")
	id = alice.Download("bob", `Music\Boards of Canada\Geogaddi (2002)\cover.jpg`, 4)
	waitFor(t, "the pierced download", 20*time.Second, func() bool { return download(alice, id).State == Finished })

	// Downloads are saved, and a new client resumes the list.
	alice.Stop()
	again := New(alice.Config(), filepath.Join(tmp, "alice"))
	if len(again.Downloads()) != 3 {
		t.Fatalf("saved %d downloads", len(again.Downloads()))
	}
}

func TestMatch(t *testing.T) {
	s := &Shares{byPath: map[string]int32{}, words: map[string][]int32{}, byDir: map[string][]int32{}}
	for i, p := range []string{`Music\Aphex Twin\SAW 85-92\01 Xtal.flac`, `Music\Aphex Twin\Drukqs\Avril 14th.mp3`,
		`Music\Other\Xtal (cover).mp3`} {
		s.files = append(s.files, shared{vpath: p})
		for _, w := range words(p) {
			l := s.words[w]
			if len(l) == 0 || l[len(l)-1] != int32(i) {
				s.words[w] = append(l, int32(i))
			}
		}
	}
	names := func(q string, ex []string) string {
		var out []string
		for _, f := range s.Match(q, ex, 10) {
			out = append(out, f.Name)
		}
		return strings.Join(out, "|")
	}
	for _, c := range []struct{ q, want string }{
		{"xtal", `Music\Aphex Twin\SAW 85-92\01 Xtal.flac|Music\Other\Xtal (cover).mp3`},
		{"XTAL -cover", `Music\Aphex Twin\SAW 85-92\01 Xtal.flac`},
		{"aphex *ukq", `Music\Aphex Twin\Drukqs\Avril 14th.mp3`},
		{"*tal", ``},
		{"nothing", ``},
	} {
		if got := names(c.q, nil); got != c.want {
			t.Errorf("%q: %q, want %q", c.q, got, c.want)
		}
	}
	if got := names("xtal", []string{"saw 85"}); got != `Music\Other\Xtal (cover).mp3` {
		t.Errorf("excluded phrase: %q", got)
	}
}

func TestMP3Attrs(t *testing.T) {
	// MPEG-1 layer III, 128 kbit/s, 44.1 kHz, joint stereo, after a 100-byte ID3v2 tag.
	b := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x5a"), make([]byte, 90)...)
	b = append(b, 0xff, 0xfb, 0x90, 0x44)
	b = append(b, make([]byte, 400)...)
	a := mp3Attrs(b, 100+128000/8*240) // four minutes of 128 kbit/s after the tag
	if len(a) != 3 || a[0] != (Attr{attrBitrate, 128}) || a[1] != (Attr{attrDuration, 240}) || a[2].Value != 0 {
		t.Fatalf("%+v", a)
	}
	// A Xing header: 9188 frames of 1152 samples is 240 s.
	x := append([]byte{}, b[:104]...)
	x = append(x, make([]byte, 32)...)
	x = append(x, "Xing"...)
	x = binary.BigEndian.AppendUint32(x, 1)
	x = binary.BigEndian.AppendUint32(x, 9188)
	x = append(x, make([]byte, 100)...)
	a = mp3Attrs(x, 100+6_000_000)
	if len(a) != 3 || a[1].Value != 240 || a[2].Value != 1 || a[0].Value != 200 {
		t.Fatalf("xing %+v", a)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{`AC/DC: "Back" <In> Black?`: `AC_DC_ _Back_ _In_ Black_`, "x. ": "x",
		"": "_", "caf\xe9": "café"} {
		if got := safeName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// response is a user's answer to search token: n files (and priv private ones), each a FLAC with its
// rate and depth, compressed as servePeer hands it over.
func response(user string, token uint32, n, priv int, free bool, speed, queue uint32) *reader {
	list := func(k int, tag string) []File {
		files := make([]File, k)
		for i := range files {
			files[i] = File{Name: `@@music\` + user + `\Album ` + itoa(i/12) + `\` + tag + itoa(i) + ".flac", Size: uint64(i + 1),
				Ext: "flac", Attrs: []Attr{{attrSampleRate, 96000}, {attrBitDepth, 24}}}
		}
		return files
	}
	m := (&msg{}).str(user).u32(token)
	writeFiles(m, list(n, "song "))
	m.boolean(free).u32(speed).u32(queue).u32(0)
	writeFiles(m, list(priv, "private "))
	return &reader{b: compress(m.b)}
}

// A search keeps maxUserResults files a user and maxSearchResults in all, reading each answer as
// it inflates; what follows a user's long list (the free slot, speed, queue, private files) still
// reads right after the files it steps over.
func TestSearchResponseLimits(t *testing.T) {
	c := New(Config{}, t.TempDir())
	c.searches = []*Search{{Token: 7, Query: "x", Started: time.Now(), users: map[string]bool{}}}
	c.searchResponse("long", response("long", 7, 1000, 5, true, 5000, 0))
	_, res, _ := c.Results(7, 0)
	if len(res) != maxUserResults || res[0].Name != "song 0.flac" || res[0].Rate != 96000 || res[0].Depth != 24 ||
		!res[0].Free || res[0].Speed != 5000 || res[149].Name != "song 149.flac" || res[149].Size != 150 {
		t.Fatalf("a long answer: %d, %+v ... %+v", len(res), res[0], res[len(res)-1])
	}
	c.searchResponse("long", response("long", 7, 10, 0, true, 1, 0)) // a second answer from the same user
	c.searchResponse("x", response("short", 8, 10, 0, true, 1, 0))   // a search we don't have
	c.searchResponse("few", response("few", 7, 3, 2, false, 300, 4))
	_, res, _ = c.Results(7, 0)
	if len(res) != maxUserResults+5 {
		t.Fatalf("%d results", len(res))
	}
	if r := res[maxUserResults+3]; r.User != "few" || r.Name != "private 0.flac" || !r.Private || r.Free || r.Queue != 4 || r.Speed != 300 {
		t.Fatalf("a private file %+v", r)
	}
	for i := 0; len(res) < maxSearchResults && i < 40; i++ {
		c.searchResponse("", response("u"+itoa(i), 7, 1000, 0, true, 1, 0))
		_, res, _ = c.Results(7, 0)
	}
	if len(res) != maxSearchResults || c.room(7, "late") != 0 {
		t.Fatalf("a full search: %d", len(res))
	}
	c.searchResponse("late", response("late", 7, 10, 0, true, 1, 0))
	c.CloseSearch(7)
	c.searchResponse("closed", response("closed", 7, 10, 0, true, 1, 0))
	if _, res, _ = c.Results(7, 0); res != nil {
		t.Fatal("a closed search kept results")
	}
}

// A new battery level or cable is a change the phone hears of, as sharing starting is.
func TestPowerChange(t *testing.T) {
	setPower(t, false, 60)
	t.Cleanup(func() { os.Remove(power.Gauge) })
	c := New(Config{}, t.TempDir())
	c.updateSharing()
	ch := c.Changed()
	c.updateSharing()
	select {
	case <-ch:
		t.Fatal("a change without one")
	default:
	}
	os.WriteFile(power.Gauge, []byte("59\n"), 0o644)
	c.updateSharing()
	select {
	case <-ch:
	default:
		t.Fatal("no change for a new level")
	}
	if st := c.Status(); st.Battery != 59 || st.External {
		t.Fatalf("status %+v", st)
	}
}
