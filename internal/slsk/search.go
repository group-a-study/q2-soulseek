package slsk

import (
	"errors"
	"strings"
	"time"
)

const (
	maxSearches      = 8
	maxSearchResults = 3000
	maxUserResults   = 500
)

// Search is one of our searches and what has come back for it.
type Search struct {
	Token   uint32
	Query   string
	Started time.Time
	Results []Result
	users   map[string]bool
}

// Result is a file a user offered for a search.
type Result struct {
	N        int    `json:"n"` // its place in the search's results, which never changes
	User     string `json:"user"`
	Name     string `json:"name"`   // the file name
	Folder   string `json:"folder"` // its folder
	Size     uint64 `json:"size"`
	Ext      string `json:"ext"`
	Bitrate  uint32 `json:"bitrate,omitempty"`
	Duration uint32 `json:"duration,omitempty"`
	VBR      bool   `json:"vbr,omitempty"`
	Rate     uint32 `json:"rate,omitempty"`
	Depth    uint32 `json:"depth,omitempty"`
	Free     bool   `json:"free"`
	Speed    uint32 `json:"speed"`
	Queue    uint32 `json:"queue"`
	Private  bool   `json:"private,omitempty"`

	user, file string // the bytes as sent, for the download
}

// SearchInfo is a search as the phone lists it.
type SearchInfo struct {
	Token   uint32 `json:"id"`
	Query   string `json:"query"`
	Results int    `json:"results"`
	Users   int    `json:"users"`
	Started int64  `json:"started"`
}

// Search sends query to the network; results come in over the next minutes.
func (c *Client) Search(query string) (uint32, error) {
	query = strings.Join(strings.Fields(query), " ")
	if len([]rune(query)) < 2 {
		return 0, errors.New("type at least two letters")
	}
	c.mu.Lock()
	online := c.server != nil
	c.mu.Unlock()
	if !online {
		return 0, errors.New("not connected to Soulseek")
	}
	token := c.nextToken()
	c.mu.Lock()
	c.searches = append(c.searches, &Search{Token: token, Query: query, Started: time.Now(), users: map[string]bool{}})
	if len(c.searches) > maxSearches {
		c.searches = c.searches[1:]
	}
	c.notify()
	c.mu.Unlock()
	c.serverSend(coded(sFileSearch).u32(token).str(query))
	return token, nil
}

// Searches lists our searches, newest first.
func (c *Client) Searches() []SearchInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SearchInfo, 0, len(c.searches))
	for i := len(c.searches) - 1; i >= 0; i-- {
		s := c.searches[i]
		out = append(out, SearchInfo{s.Token, s.Query, len(s.Results), len(s.users), s.Started.Unix()})
	}
	return out
}

// Results is a search's results from the from-th on; ok is false for a search no longer kept.
func (c *Client) Results(token uint32, from int) (SearchInfo, []Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.searches {
		if s.Token == token {
			info := SearchInfo{s.Token, s.Query, len(s.Results), len(s.users), s.Started.Unix()}
			if from < 0 || from > len(s.Results) {
				from = len(s.Results)
			}
			return info, append([]Result(nil), s.Results[from:]...), true
		}
	}
	return SearchInfo{}, nil, false
}

// Result is one result, for a download from the phone or the Q2.
func (c *Client) Result(token uint32, n int) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.searches {
		if s.Token == token && n >= 0 && n < len(s.Results) {
			return s.Results[n], true
		}
	}
	return Result{}, false
}

// RecentSearch is a search for query started within age, so the Q2's page can come back to it.
func (c *Client) RecentSearch(query string, age time.Duration) (uint32, bool) {
	query = strings.Join(strings.Fields(query), " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.searches) - 1; i >= 0; i-- {
		if s := c.searches[i]; strings.EqualFold(s.Query, query) && time.Since(s.Started) < age {
			return s.Token, true
		}
	}
	return 0, false
}

// CloseSearch forgets a search; its later results are dropped.
func (c *Client) CloseSearch(token uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.searches {
		if s.Token == token {
			c.searches = append(c.searches[:i], c.searches[i+1:]...)
			c.notify()
			return
		}
	}
}

// DownloadResult queues a result's file.
func (r Result) Download(c *Client) int { return c.Download(r.user, r.file, int64(r.Size)) }

// File is the result's file as the user sent it, for a download.
func (r Result) File() File { return File{Name: r.file, Size: r.Size} }

// FolderOf is the result's folder as the user shares it, and its user, both as sent.
func (r Result) FolderOf() (user, folder string) {
	dir, _ := splitRemote(r.file)
	return r.user, dir
}

func (c *Client) searchResponse(from string, r *reader) {
	raw, err := decompress(r.b, 16<<20)
	if err != nil {
		return
	}
	r = &reader{b: raw}
	user := r.str()
	token := r.u32()
	files := readFiles(r, 100000)
	free := r.boolean()
	speed := r.u32()
	queue := r.u32()
	if r.err != nil {
		return
	}
	r.u32()
	var private []File
	if len(r.b) >= 4 {
		private = readFiles(r, 100000)
	}
	if user == "" {
		user = from
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var s *Search
	for _, x := range c.searches {
		if x.Token == token {
			s = x
		}
	}
	if s == nil || s.users[user] {
		return
	}
	s.users[user] = true
	add := func(f File, priv bool) {
		if len(s.Results) >= maxSearchResults {
			return
		}
		dir, name := splitRemote(f.Name)
		res := Result{N: len(s.Results), User: Text(user), Name: Text(name), Folder: Text(dir), Size: f.Size,
			Ext: strings.ToLower(Text(f.Ext)), Free: free, Speed: speed, Queue: queue, Private: priv, user: user, file: f.Name}
		if res.Ext == "" {
			if i := strings.LastIndexByte(res.Name, '.'); i >= 0 {
				res.Ext = strings.ToLower(res.Name[i+1:])
			}
		}
		if v, ok := f.attr(attrBitrate); ok {
			res.Bitrate = v
		}
		if v, ok := f.attr(attrDuration); ok {
			res.Duration = v
		}
		if v, ok := f.attr(attrVBR); ok {
			res.VBR = v == 1
		}
		if v, ok := f.attr(attrSampleRate); ok {
			res.Rate = v
		}
		if v, ok := f.attr(attrBitDepth); ok {
			res.Depth = v
		}
		s.Results = append(s.Results, res)
	}
	for i, f := range files {
		if i < maxUserResults {
			add(f, false)
		}
	}
	for i, f := range private {
		if i < maxUserResults {
			add(f, true)
		}
	}
	c.notify()
}

// searchRequest answers someone's search with our matching files, while sharing is active.
func (c *Client) searchRequest(user string, token uint32, query string) {
	c.mu.Lock()
	me := c.cfg.Username
	if !c.shareActive || c.shares == nil || user == me || len(query) < 3 {
		c.mu.Unlock()
		return
	}
	files := c.shares.Match(query, c.excluded, c.cfg.MaxResults)
	if len(files) == 0 {
		c.mu.Unlock()
		return
	}
	active, queued := 0, 0
	for _, u := range c.ups {
		switch u.State {
		case Queued:
			queued++
		case Requesting, Starting, Uploading:
			active++
		}
	}
	free := active < c.cfg.Slots
	speed := uint32(c.uploadSpeed)
	c.mu.Unlock()
	m := (&msg{}).str(me).u32(token)
	writeFiles(m, files)
	m.boolean(free).u32(speed).u32(uint32(queued)).u32(0).u32(0)
	c.sendPeer(user, coded(pSearchResponse).bytesRaw(compress(m.b)))
}
