package slsk

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/group-a-study/q2-soulseek/internal/q2"
)

// Transfer states, as the phone and the Q2 show them.
const (
	Queued      = "Queued"     // a download waiting to ask its user; an upload waiting for a slot
	Requesting  = "Requesting" // asking the user to queue it (download), or offering it (upload)
	Remote      = "In queue"   // the user has our download in their queue
	Starting    = "Starting"   // the file connection is being made
	Downloading = "Downloading"
	Uploading   = "Uploading"
	Finished    = "Finished"
	Paused      = "Paused"
	Failed      = "Failed"
)

const (
	incompleteDir = ".incomplete"
	chunkWrite    = 512 << 10 // bytes gathered before one open-write-close on the card
	chunkRead     = 256 << 10
	maxFinished   = 500
	maxUploadList = 2000
	maxUserQueue  = 300
)

type Transfer struct {
	ID    int
	Up    bool
	User  string
	File  string // the peer's virtual path, its bytes as sent
	Size  int64
	Local string // a finished download's file; an upload's shared file
	State string
	Err   string
	Bytes int64
	Speed float64 // bytes a second
	Place int
	Added time.Time
	Done  time.Time

	token   uint32
	tries   int
	next    time.Time // a queued download asks again after this
	asked   time.Time // QueueUpload last sent
	startAt time.Time
	conn    net.Conn
	resp    chan transferAnswer
	removed bool
	rateAt  time.Time
	rateB   int64
}

type transferAnswer struct {
	allowed bool
	reason  string
}

// Info is a transfer as the phone sees it.
type Info struct {
	ID     int     `json:"id"`
	Up     bool    `json:"up,omitempty"`
	User   string  `json:"user"`
	Name   string  `json:"name"`   // the file's name
	Folder string  `json:"folder"` // its folder, as the user shares it
	Size   int64   `json:"size"`
	Bytes  int64   `json:"bytes"`
	Speed  float64 `json:"speed"`
	State  string  `json:"state"`
	Error  string  `json:"error,omitempty"`
	Place  int     `json:"place,omitempty"`
	Path   string  `json:"path,omitempty"`
	Added  int64   `json:"added"`
}

func (t *Transfer) info() Info {
	dir, name := splitRemote(t.File)
	path := ""
	if t.State == Finished && !t.Up {
		path = strings.TrimPrefix(t.Local, q2.Card)
	}
	return Info{ID: t.ID, Up: t.Up, User: Text(t.User), Name: Text(name), Folder: Text(dir), Size: t.Size, Bytes: t.Bytes,
		Speed: t.Speed, State: t.State, Error: t.Err, Place: t.Place, Path: path, Added: t.Added.Unix()}
}

// splitRemote is a virtual path's folder and file name.
func splitRemote(p string) (string, string) {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// Downloads lists the downloads, newest first.
func (c *Client) Downloads() []Info { return c.list(false) }

// Uploads lists the uploads, newest first.
func (c *Client) Uploads() []Info { return c.list(true) }

func (c *Client) list(up bool) []Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	src := c.downs
	if up {
		src = c.ups
	}
	out := make([]Info, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		out = append(out, src[i].info())
	}
	return out
}

// Download queues a file of user's. A file already being downloaded isn't added twice.
func (c *Client) Download(user, file string, size int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		if d.User == user && d.File == file && d.State != Finished && d.State != Failed {
			return d.ID
		}
	}
	c.nextID++
	d := &Transfer{ID: c.nextID, User: user, File: file, Size: size, State: Queued, Added: time.Now()}
	c.downs = append(c.downs, d)
	c.trimDownloads()
	c.dirty = true
	c.notify()
	return d.ID
}

// DownloadFolder asks user for a folder (with its subfolders) and queues every file in it. Without an
// answer, the files given (those a search showed) are queued instead.
func (c *Client) DownloadFolder(user, folder string, known []File) {
	go func() {
		dirs, err := c.FolderFiles(user, folder, 30*time.Second)
		var files []File
		if err == nil {
			names := make([]string, 0, len(dirs))
			for d := range dirs {
				names = append(names, d)
			}
			sort.Strings(names)
			for _, d := range names {
				for _, f := range dirs[d] {
					name := f.Name
					if !strings.Contains(name, `\`) {
						name = d + `\` + name
					}
					files = append(files, File{Name: name, Size: f.Size})
				}
			}
		}
		if len(files) == 0 {
			files = known
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		for _, f := range files {
			c.Download(user, f.Name, int64(f.Size))
		}
	}()
}

func (c *Client) find(id int) *Transfer {
	for _, l := range [][]*Transfer{c.downs, c.ups} {
		for _, t := range l {
			if t.ID == id {
				return t
			}
		}
	}
	return nil
}

// Action does pause, resume, retry or remove to a transfer.
func (c *Client) Action(id int, action string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.find(id)
	if t == nil {
		return errors.New("no such transfer")
	}
	switch action {
	case "pause":
		if t.Up || t.State == Finished {
			return errors.New("can't pause this")
		}
		t.State, t.Err, t.Speed = Paused, "", 0
		if t.conn != nil {
			t.conn.Close()
		}
	case "resume", "retry":
		if t.State == Finished && (t.Up || fileExists(t.Local)) {
			return errors.New("already finished")
		}
		if t.Up {
			t.State, t.Err = Queued, ""
			break
		}
		t.State, t.Err, t.tries, t.next = Queued, "", 0, time.Time{}
	case "remove":
		if t.conn != nil {
			t.conn.Close()
		}
		t.removed = true
		if !t.Up && t.State != Finished {
			os.Remove(c.partPath(t))
		}
		c.drop(t)
	default:
		return errors.New("unknown action")
	}
	if !t.Up {
		c.dirty = true
	}
	c.notify()
	return nil
}

// Clear removes finished (or failed) downloads from the list; their files stay.
func (c *Client) Clear(which string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.downs[:0]
	for _, d := range c.downs {
		if (which == "finished" && d.State == Finished) || (which == "failed" && d.State == Failed) {
			if d.State == Failed {
				os.Remove(c.partPath(d))
			}
			d.removed = true
			continue
		}
		kept = append(kept, d)
	}
	c.downs = kept
	c.dirty = true
	c.notify()
}

func (c *Client) drop(t *Transfer) {
	l := &c.downs
	if t.Up {
		l = &c.ups
	}
	for i, x := range *l {
		if x == t {
			*l = append((*l)[:i], (*l)[i+1:]...)
			return
		}
	}
}

// trimDownloads keeps at most maxFinished finished ones, dropping the oldest.
func (c *Client) trimDownloads() {
	n := 0
	for _, d := range c.downs {
		if d.State == Finished {
			n++
		}
	}
	for i := 0; n > maxFinished && i < len(c.downs); {
		if c.downs[i].State == Finished {
			c.downs = append(c.downs[:i], c.downs[i+1:]...)
			n--
			continue
		}
		i++
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// safeName makes a peer's name a card-safe one: FAT's reserved characters and controls become _,
// trailing dots and spaces go, and it is cut to 200 bytes on a character boundary.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) || r == utf8.RuneError {
			return '_'
		}
		return r
	}, Text(s))
	if len(s) > 200 {
		n := 200
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return "_"
	}
	return s
}

func (c *Client) partPath(d *Transfer) string {
	_, name := splitRemote(d.File)
	return filepath.Join(c.cfg.DownloadDir, incompleteDir, safeName(itoa(d.ID)+"-"+name))
}

// destPath is where a finished download goes: the download folder, then the folder the file was
// in, as Nicotine+ does, without overwriting a file already there.
func (c *Client) destPath(d *Transfer) string {
	dir, name := splitRemote(d.File)
	_, parent := splitRemote(dir)
	base := c.cfg.DownloadDir
	if parent != "" {
		base = filepath.Join(base, safeName(parent))
	}
	p := filepath.Join(base, safeName(name))
	ext := filepath.Ext(p)
	for n := 2; fileExists(p); n++ {
		p = filepath.Join(base, strings.TrimSuffix(safeName(name), ext)+" ("+itoa(n)+")"+ext)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func retryIn(tries int) time.Duration {
	d := 30 * time.Second << min(tries, 5)
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// resumeTransfers asks again for every download in flight: after a login the peers' queues are
// what they were, but our connections are gone.
func (c *Client) resumeTransfers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		switch d.State {
		case Requesting, Remote, Starting, Downloading:
			d.State, d.next, d.Speed = Queued, time.Time{}, 0
		}
	}
	for _, u := range c.ups {
		if u.State == Requesting || u.State == Starting || u.State == Uploading {
			u.State = Queued
		}
	}
	c.notify()
}

// transferLoop asks for queued downloads, gives up on transfers that never connected, starts uploads
// in free slots while sharing is active, and saves the downloads.
func (c *Client) transferLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		c.mu.Lock()
		now := time.Now()
		online := c.server != nil
		var ask, upl []*Transfer
		requesting := 0
		for _, d := range c.downs {
			if d.State == Requesting {
				requesting++
			}
		}
		for _, d := range c.downs {
			switch d.State {
			case Queued:
				if online && now.After(d.next) && requesting < 6 {
					d.State, d.Err = Requesting, ""
					requesting++
					ask = append(ask, d)
				}
			case Remote:
				if online && now.Sub(d.asked) > 10*time.Minute {
					d.asked = now
					ask = append(ask, d)
				}
			case Starting:
				if now.Sub(d.startAt) > 60*time.Second {
					d.State, d.Err, d.next = Queued, "The user couldn't connect to send it", now.Add(retryIn(d.tries))
					d.tries++
					c.notify()
				}
			}
		}
		if online && c.shareActive {
			active, busy := 0, map[string]bool{}
			for _, u := range c.ups {
				if u.State == Requesting || u.State == Starting || u.State == Uploading {
					active++
					busy[u.User] = true
				}
			}
			for active < c.cfg.Slots {
				var next *Transfer
				for _, u := range c.ups {
					if u.State == Queued && !busy[u.User] {
						next = u
						break
					}
				}
				if next == nil {
					break
				}
				next.State, next.Err = Requesting, ""
				busy[next.User] = true
				active++
				upl = append(upl, next)
			}
		}
		if len(ask)+len(upl) > 0 {
			c.notify()
		}
		c.mu.Unlock()
		for _, d := range ask {
			go c.ask(d)
		}
		for _, u := range upl {
			go c.upload(u)
		}
		c.saveDownloads()
	}
}

// ask sends a QueueUpload (and asks our place) for a download.
func (c *Client) ask(d *Transfer) {
	c.mu.Lock()
	user, file := d.User, d.File
	c.mu.Unlock()
	err := c.sendPeer(user, coded(pQueueUpload).str(file))
	if err == nil {
		c.sendPeer(user, coded(pPlaceInQueueReq).str(file))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.removed {
		return
	}
	now := time.Now()
	switch {
	case d.State != Requesting && d.State != Remote:
	case err != nil:
		d.State, d.next = Queued, now.Add(retryIn(d.tries))
		d.tries++
		d.Err = "Can't reach the user"
		if err == errOffline {
			d.Err = "The user is offline"
		}
	default:
		d.State, d.asked, d.Err = Remote, now, ""
	}
	c.dirty = true
	c.notify()
}

// transferRequest is a peer ready to upload a file to us (direction 1), or an old-style request to
// download one of ours (direction 0), which goes in the upload queue.
func (c *Client) transferRequest(p *conn, dir, token uint32, file string, size uint64) {
	if dir == 0 {
		p.send(coded(pTransferResponse).u32(token).boolean(false).str("Queued"))
		c.queueUpload(p, file)
		return
	}
	c.mu.Lock()
	var d *Transfer
	for _, x := range c.downs {
		if x.User == p.user && x.File == file && x.State != Finished && x.State != Paused && !x.removed {
			d = x
			break
		}
	}
	if d == nil {
		c.mu.Unlock()
		p.send(coded(pTransferResponse).u32(token).boolean(false).str("Cancelled"))
		return
	}
	d.token, d.Size, d.State, d.startAt, d.Err, d.Place = token, int64(size), Starting, time.Now(), "", 0
	c.dirty = true
	c.notify()
	c.mu.Unlock()
	p.send(coded(pTransferResponse).u32(token).boolean(true))
}

// fileIncoming is a file connection a peer opened to us: an upload to us, named by the token of its
// TransferRequest.
func (c *Client) fileIncoming(user string, nc net.Conn) {
	var tb [4]byte
	nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(nc, tb[:]); err != nil {
		nc.Close()
		return
	}
	token := binary.LittleEndian.Uint32(tb[:])
	c.mu.Lock()
	var d *Transfer
	for _, x := range c.downs {
		if x.User == user && x.token == token && x.State == Starting && !x.removed {
			d = x
			break
		}
	}
	if d != nil {
		d.conn = nc
	}
	c.mu.Unlock()
	if d == nil {
		nc.Close()
		return
	}
	c.receive(d, nc)
}

// receive is a download's bytes: our offset (what an earlier try left), then the rest of the file,
// gathered and written to the card in chunks so the file is never left open.
func (c *Client) receive(d *Transfer, nc net.Conn) {
	defer nc.Close()
	c.mu.Lock()
	part := c.partPath(d)
	size := d.Size
	c.mu.Unlock()
	os.MkdirAll(filepath.Dir(part), 0o755)
	off := int64(0)
	if st, err := os.Stat(part); err == nil {
		if st.Size() <= size {
			off = st.Size()
		} else {
			os.Remove(part)
		}
	}
	var ob [8]byte
	binary.LittleEndian.PutUint64(ob[:], uint64(off))
	nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := nc.Write(ob[:]); err != nil {
		c.downloadEnded(d, errors.New("the connection closed"))
		return
	}
	c.mu.Lock()
	d.State, d.Bytes, d.rateAt, d.rateB = Downloading, off, time.Now(), off
	c.notify()
	c.mu.Unlock()
	if size == 0 {
		q2.Append(part, 0, nil)
	}
	buf := make([]byte, 0, chunkWrite)
	chunk := make([]byte, 64<<10)
	got := off
	var err error
	for got+int64(len(buf)) < size {
		nc.SetReadDeadline(time.Now().Add(90 * time.Second))
		want := min(int64(len(chunk)), size-got-int64(len(buf)))
		var n int
		n, err = nc.Read(chunk[:want])
		buf = append(buf, chunk[:n]...)
		if len(buf) >= chunkWrite || got+int64(len(buf)) == size || (err != nil && len(buf) > 0) {
			if werr := q2.Append(part, got, buf); werr != nil {
				err = werr
				break
			}
			got += int64(len(buf))
			buf = buf[:0]
			c.progress(d, got)
		}
		if err != nil {
			break
		}
	}
	if got == size {
		c.finish(d, part)
		return
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("the connection closed")
	}
	c.downloadEnded(d, err)
}

func (c *Client) progress(t *Transfer, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Bytes = bytes
	if dt := time.Since(t.rateAt); dt >= time.Second {
		inst := float64(bytes-t.rateB) / dt.Seconds()
		if t.Speed == 0 {
			t.Speed = inst
		} else {
			t.Speed = t.Speed*0.6 + inst*0.4
		}
		t.rateAt, t.rateB = time.Now(), bytes
		c.notify()
	}
}

// downloadEnded is a download's connection gone before the end: it asks again shortly, unless it
// was paused or removed meanwhile.
func (c *Client) downloadEnded(d *Transfer, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d.conn, d.Speed = nil, 0
	if d.removed || d.State == Paused {
		c.notify()
		return
	}
	d.State, d.next = Queued, time.Now().Add(20*time.Second)
	d.Err = "Interrupted: " + err.Error()
	if errors.Is(err, q2.ErrNoCard) {
		d.Err, d.next = "The card is away (USB storage)", time.Now().Add(time.Minute)
	}
	c.dirty = true
	c.notify()
}

func (c *Client) finish(d *Transfer, part string) {
	c.mu.Lock()
	dest := c.destPath(d)
	c.mu.Unlock()
	os.MkdirAll(filepath.Dir(dest), 0o755)
	err := os.Rename(part, dest)
	c.mu.Lock()
	d.conn, d.Speed = nil, 0
	if err != nil {
		d.State, d.Err = Failed, "Couldn't save the file: "+err.Error()
	} else {
		d.State, d.Local, d.Done, d.Err, d.Bytes = Finished, dest, time.Now(), "", d.Size
		log.Printf("downloaded %s", Text(d.File))
	}
	c.trimDownloads()
	c.dirty = true
	c.notify()
	c.mu.Unlock()
}

func (c *Client) placeInQueue(user, file string, place int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		if d.User == user && d.File == file && !d.removed {
			d.Place = place
			if d.State == Requesting || d.State == Queued {
				d.State, d.asked = Remote, time.Now()
			}
			c.notify()
		}
	}
}

// uploadFailed is the peer's upload to us failing on their side: ask again.
func (c *Client) uploadFailed(user, file string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		if d.User == user && d.File == file && !d.removed &&
			(d.State == Starting || d.State == Downloading || d.State == Remote) {
			if d.conn != nil {
				d.conn.Close()
			}
			d.State, d.next, d.Err = Queued, time.Now().Add(30*time.Second), "The user's upload failed"
			c.dirty = true
			c.notify()
		}
	}
}

// uploadDenied is the peer refusing our download.
func (c *Client) uploadDenied(user, file, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.downs {
		if d.User != user || d.File != file || d.removed || d.State == Finished || d.State == Paused {
			continue
		}
		switch reason {
		case "Queued":
			d.State, d.asked = Remote, time.Now()
			continue
		case "Pending shutdown.":
			d.State, d.next, d.Err = Queued, time.Now().Add(10*time.Minute), "The user is shutting down"
			continue
		case "File not shared.", "File not shared":
			d.Err = "The user doesn't share this file"
		case "Too many files", "Too many megabytes":
			d.Err = "The user's queue for you is full"
		case "Banned":
			d.Err = "The user won't share with you"
		default:
			d.Err = Text(reason)
		}
		d.State = Failed
		c.dirty = true
		c.notify()
	}
}

// queueUpload is a peer asking for one of our files.
func (c *Client) queueUpload(p *conn, file string) {
	c.mu.Lock()
	path, size, ok := c.shares.Lookup(file)
	if c.cfg.Sharing == ShareOff || !ok {
		c.mu.Unlock()
		p.send(coded(pUploadDenied).str(file).str("File not shared."))
		return
	}
	mine, total := 0, 0
	for _, u := range c.ups {
		if u.State == Queued || u.State == Requesting || u.State == Starting || u.State == Uploading {
			if u.User == p.user && u.File == file {
				c.mu.Unlock()
				return
			}
			total++
			if u.User == p.user {
				mine++
			}
		}
	}
	if mine >= maxUserQueue || total >= maxUploadList {
		c.mu.Unlock()
		p.send(coded(pUploadDenied).str(file).str("Too many files"))
		return
	}
	c.nextID++
	c.ups = append(c.ups, &Transfer{ID: c.nextID, Up: true, User: p.user, File: file, Size: size, Local: path,
		State: Queued, Added: time.Now()})
	// Keep the last 100 ended uploads to show.
	ended := 0
	for i := len(c.ups) - 1; i >= 0; i-- {
		if s := c.ups[i].State; s == Finished || s == Failed {
			if ended++; ended > 100 {
				c.ups = append(c.ups[:i], c.ups[i+1:]...)
			}
		}
	}
	c.notify()
	c.mu.Unlock()
}

func (c *Client) placeRequest(p *conn, file string) {
	c.mu.Lock()
	place := 0
	for _, u := range c.ups {
		if u.State == Queued {
			place++
			if u.User == p.user && u.File == file {
				c.mu.Unlock()
				p.send(coded(pPlaceInQueue).str(file).u32(uint32(place)))
				return
			}
		}
	}
	c.mu.Unlock()
}

func (c *Client) transferResponse(token uint32, allowed bool, reason string) {
	c.mu.Lock()
	u := c.upTokens[token]
	c.mu.Unlock()
	if u != nil {
		select {
		case u.resp <- transferAnswer{allowed, reason}:
		default:
		}
	}
}

// upload offers a queued file to its user and, once they take it, sends it on a file connection.
func (c *Client) upload(u *Transfer) {
	token := c.nextToken()
	c.mu.Lock()
	u.token, u.resp = token, make(chan transferAnswer, 1)
	c.upTokens[token] = u
	user, file, size, path := u.User, u.File, u.Size, u.Local
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.upTokens, token)
		u.conn, u.Speed = nil, 0
		c.notify()
		c.mu.Unlock()
	}()
	fail := func(state, why string) {
		c.mu.Lock()
		if !u.removed && u.State != Queued {
			u.State, u.Err = state, why
		}
		c.mu.Unlock()
	}
	if err := c.sendPeer(user, coded(pTransferRequest).u32(1).u32(token).str(file).u64(uint64(size))); err != nil {
		fail(Failed, "The user is offline")
		return
	}
	var ans transferAnswer
	select {
	case ans = <-u.resp:
	case <-time.After(60 * time.Second):
		fail(Failed, "The user didn't answer")
		return
	}
	if !ans.allowed {
		fail(Failed, Text(ans.reason))
		return
	}
	c.mu.Lock()
	if !c.shareActive || u.removed {
		c.mu.Unlock()
		return
	}
	u.State = Starting
	c.notify()
	c.mu.Unlock()
	nc, err := c.dial(user, 'F')
	if err != nil {
		fail(Failed, "Can't connect to the user")
		return
	}
	defer nc.Close()
	var tb [4]byte
	binary.LittleEndian.PutUint32(tb[:], token)
	var ob [8]byte
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := nc.Write(tb[:]); err != nil {
		fail(Failed, "The connection closed")
		return
	}
	if _, err := io.ReadFull(nc, ob[:]); err != nil {
		fail(Failed, "The user didn't start the download")
		return
	}
	off := int64(binary.LittleEndian.Uint64(ob[:]))
	if off < 0 || off > size {
		off = 0
	}
	c.mu.Lock()
	if !c.shareActive || u.removed {
		c.mu.Unlock()
		return
	}
	u.State, u.Bytes, u.conn, u.rateAt, u.rateB = Uploading, off, nc, time.Now(), off
	c.notify()
	c.mu.Unlock()
	buf := make([]byte, chunkRead)
	sent := off
	for sent < size {
		n, err := q2.ReadAt(path, sent, buf[:min(int64(len(buf)), size-sent)])
		if err != nil || n == 0 {
			c.sendPeer(user, coded(pUploadFailed).str(file))
			fail(Failed, "Couldn't read the file")
			return
		}
		for k := 0; k < n; {
			step := min(n-k, 32<<10)
			c.bucket.take(step, c.Config().UploadKBps)
			nc.SetWriteDeadline(time.Now().Add(90 * time.Second))
			w, err := nc.Write(buf[k : k+step])
			k += w
			sent += int64(w)
			if err != nil {
				c.mu.Lock()
				active := c.shareActive
				c.mu.Unlock()
				if active {
					c.sendPeer(user, coded(pUploadFailed).str(file))
					fail(Failed, "The connection closed")
				}
				return
			}
		}
		c.progress(u, sent)
	}
	// The downloader closes the connection once it has everything.
	nc.SetReadDeadline(time.Now().Add(60 * time.Second))
	io.Copy(io.Discard, nc)
	c.mu.Lock()
	u.State, u.Done, u.Err = Finished, time.Now(), ""
	c.mu.Unlock()
	log.Printf("uploaded %s to %s", Text(file), Text(user))
}

// bucket paces uploads to the configured rate, shared by every upload.
type bucket struct {
	mu   sync.Mutex
	next time.Time
}

func newBucket() *bucket { return &bucket{} }

func (b *bucket) take(n, kbps int) {
	if kbps <= 0 {
		return
	}
	b.mu.Lock()
	now := time.Now()
	if b.next.Before(now) {
		b.next = now
	}
	at := b.next
	b.next = b.next.Add(time.Duration(float64(n) / float64(kbps*1024) * float64(time.Second)))
	b.mu.Unlock()
	time.Sleep(time.Until(at))
}

// saved is a download as downloads.json keeps it; names are bytes, as peers' names need not be UTF-8.
type saved struct {
	ID    int       `json:"id"`
	User  []byte    `json:"user"`
	File  []byte    `json:"file"`
	Size  int64     `json:"size"`
	Local string    `json:"local,omitempty"`
	State string    `json:"state"`
	Err   string    `json:"error,omitempty"`
	Bytes int64     `json:"bytes"`
	Added time.Time `json:"added"`
	Done  time.Time `json:"done,omitempty"`
}

func (c *Client) saveDownloads() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	c.dirty = false
	list := make([]saved, 0, len(c.downs))
	for _, d := range c.downs {
		state := d.State
		if state != Finished && state != Paused && state != Failed {
			state = Queued
		}
		list = append(list, saved{d.ID, []byte(d.User), []byte(d.File), d.Size, d.Local, state, d.Err, d.Bytes, d.Added, d.Done})
	}
	c.mu.Unlock()
	b, _ := json.Marshal(list)
	if err := q2.WriteFile(filepath.Join(c.dir, "downloads.json"), b); err != nil {
		c.mu.Lock()
		c.dirty = true
		c.mu.Unlock()
	}
}

func (c *Client) loadDownloads() {
	b, err := os.ReadFile(filepath.Join(c.dir, "downloads.json"))
	if err != nil {
		return
	}
	var list []saved
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for _, s := range list {
		c.downs = append(c.downs, &Transfer{ID: s.ID, User: string(s.User), File: string(s.File), Size: s.Size,
			Local: s.Local, State: s.State, Err: s.Err, Bytes: s.Bytes, Added: s.Added, Done: s.Done})
		c.nextID = max(c.nextID, s.ID)
	}
}
