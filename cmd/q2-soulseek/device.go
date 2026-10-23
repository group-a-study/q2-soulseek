package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/q2spot"
	"github.com/group-a-study/q2-soulseek/internal/slsk"
)

// The Q2's own Soulseek page (patch/soulseek.c) is drawn from these replies: each row's kind says
// what a press does (contrib/shanlingq2/README, The Q2's page). list opens the level uri (arguments: context);
// action sends uri with context and toasts the message; search opens the search box; remote opens
// the QR code; info does nothing. A row's position, when set, is the op Play/Pause held sends.
// An ok reply whose message is "refresh" is asked again every 2 s while it is on top.

const refresh = "refresh"

var audio = map[string]bool{"mp3": true, "flac": true, "m4a": true, "ogg": true, "opus": true, "wav": true,
	"aiff": true, "aif": true, "ape": true, "wv": true, "alac": true, "aac": true, "wma": true, "dsf": true}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func quality(r slsk.Result) string {
	ext := strings.ToUpper(r.Ext)
	switch {
	case r.Depth > 0 && r.Rate > 0:
		return fmt.Sprintf("%s %d/%s", ext, r.Depth, khz(r.Rate))
	case r.Bitrate > 0 && r.VBR:
		return fmt.Sprintf("%s V%d", ext, r.Bitrate)
	case r.Bitrate > 0:
		return fmt.Sprintf("%s %d", ext, r.Bitrate)
	}
	return ext
}

func khz(rate uint32) string {
	s := strconv.FormatFloat(float64(rate)/1000, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

var sharingNames = map[string]string{slsk.ShareOff: "Off", slsk.ShareCharging: "While charging",
	slsk.ShareBattery: "Charging or battery"}

func sharingLine(st slsk.Status, min int) string {
	switch {
	case st.Sharing == slsk.ShareOff:
		return "Nothing is shared"
	case st.Scanning:
		return "Scanning shared folders…"
	case st.ShareActive:
		return fmt.Sprintf("Sharing %d files", st.SharedFiles)
	case st.Sharing == slsk.ShareCharging:
		return "Paused: on battery"
	}
	return fmt.Sprintf("Paused: battery under %d%%", min)
}

func status(t slsk.Info) string {
	switch t.State {
	case slsk.Downloading, slsk.Uploading:
		pct := 0
		if t.Size > 0 {
			pct = int(t.Bytes * 100 / t.Size)
		}
		return fmt.Sprintf("%d%% · %s/s", pct, size(int64(t.Speed)))
	case slsk.Remote:
		if t.Place > 0 {
			return fmt.Sprintf("In %s's queue, place %d", t.User, t.Place)
		}
		return "In " + t.User + "'s queue"
	case slsk.Finished:
		return "Finished · " + size(t.Size)
	case slsk.Failed, slsk.Queued:
		if t.Error != "" {
			return t.State + ": " + t.Error
		}
	}
	return t.State
}

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

func (a *app) device(req q2spot.Request) q2spot.Reply {
	c := a.client
	c.Touch()
	st := c.Status()
	set := a.settings()
	ok := func(msg string) q2spot.Reply { return q2spot.Reply{Status: q2spot.OK, Message: msg} }
	id := func() int { n, _ := strconv.Atoi(req.Arg(0)); return n }
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range []string{"menu", "search", "downloads", "transfer", "uploads", "account", "set",
			"download", "downloadfolder", "pause", "resume", "retry", "remove", "stop"} {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		return a.menu(st, set)
	case "search":
		return a.search(req.Arg(0), req.Arg(1), st)
	case "downloads":
		return a.transfers(c.Downloads(), req.Arg(0), "No downloads yet", "Search, then press a file")
	case "uploads":
		rows := []q2spot.Row{info("Sharing: "+sharingNames[st.Sharing], sharingLine(st, set.BatteryMin))}
		r := a.transfers(c.Uploads(), req.Arg(0), "No uploads yet", "Others download what you share")
		r.Rows = append(rows, r.Rows...)
		return r
	case "transfer":
		return a.transfer(id())
	case "account":
		user := set.Username
		if user == "" {
			user = "Not set up"
		}
		return q2spot.Reply{Message: refresh, Rows: []q2spot.Row{
			info(user, note(st, localIP())),
			info(fmt.Sprintf("%d files shared", st.SharedFiles), sharingLine(st, set.BatteryMin)),
			info(fmt.Sprintf("Listening on port %d", st.Port), "Forward it on your router for more results"),
			{Kind: "remote", Title: "Change on your phone", Subtitle: "Scan the code"},
		}}
	case "set":
		next := map[string]string{slsk.ShareOff: slsk.ShareCharging, slsk.ShareCharging: slsk.ShareBattery,
			slsk.ShareBattery: slsk.ShareOff}[set.Sharing]
		if err := a.update(func(s *settings) { s.Sharing = next }); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		return ok("Sharing: " + sharingNames[next])
	case "download", "downloadfolder":
		tok, n, found := strings.Cut(req.Arg(0), ".")
		t, _ := strconv.Atoi(tok)
		k, _ := strconv.Atoi(n)
		res, have := c.Result(uint32(t), k)
		if !found || !have {
			return q2spot.Fail(q2spot.Unavailable, "That result is gone; search again")
		}
		if req.Op == "download" {
			res.Download(c)
			return ok("Downloading " + res.Name)
		}
		(&web{a: a}).downloadFolder(res)
		return ok("Downloading the folder")
	case "pause", "resume", "retry", "remove":
		if err := c.Action(id(), req.Op); err != nil {
			return q2spot.Fail(q2spot.Unavailable, strings.ToUpper(err.Error()[:1])+err.Error()[1:])
		}
		return ok(map[string]string{"pause": "Paused", "resume": "Resumed", "retry": "Trying again",
			"remove": "Removed"}[req.Op])
	case "stop":
		go func() { time.Sleep(300 * time.Millisecond); a.shutdown() }()
		return ok("Soulseek stopped")
	}
	return q2spot.Fail(q2spot.Unsupported, "Update Soulseek on the card")
}

func (a *app) menu(st slsk.Status, set settings) q2spot.Reply {
	ip := localIP()
	addr := strings.TrimPrefix(strings.TrimSuffix(a.URL(false), "/"), "http://")
	if ip == "" {
		addr = "Connect the Q2 to Wi-Fi"
	}
	if st.State == slsk.StateSetup {
		return q2spot.Reply{Message: refresh, Rows: []q2spot.Row{
			{Kind: "remote", Title: "Set up on your phone", Subtitle: "Scan the code to choose a Soulseek name"},
			{Kind: "action", URI: "stop", Title: "Stop Soulseek"},
		}}
	}
	var dl []string
	if st.Downloading > 0 {
		dl = append(dl, fmt.Sprintf("%d downloading", st.Downloading))
	}
	if st.Queued > 0 {
		dl = append(dl, fmt.Sprintf("%d waiting", st.Queued))
	}
	if st.Finished > 0 {
		dl = append(dl, fmt.Sprintf("%d finished", st.Finished))
	}
	if len(dl) == 0 {
		dl = []string{"None yet"}
	}
	up := sharingLine(st, set.BatteryMin)
	if st.Uploading > 0 {
		up = fmt.Sprintf("%d uploading · %s", st.Uploading, up)
	}
	return q2spot.Reply{Message: refresh, Rows: []q2spot.Row{
		{Kind: "remote", Title: "Phone Remote", Subtitle: addr},
		{Kind: "search", Title: "Search", Subtitle: note(st, ip)},
		{Kind: "list", URI: "downloads", Title: "Downloads", Subtitle: strings.Join(dl, " · ")},
		{Kind: "list", URI: "uploads", Title: "Uploads", Subtitle: up},
		{Kind: "action", URI: "set", Context: "sharing", Title: "Sharing: " + sharingNames[set.Sharing],
			Subtitle: sharingLine(st, set.BatteryMin)},
		{Kind: "list", URI: "account", Title: "Account", Subtitle: note(st, ip)},
		{Kind: "action", URI: "stop", Title: "Stop Soulseek", Subtitle: "Downloads carry on next time"},
	}}
}

// search starts a search for term (or comes back to one from the last ten minutes) and lists its
// audio files, 50 a page from cursor.
func (a *app) search(term, cursor string, st slsk.Status) q2spot.Reply {
	c := a.client
	if st.State != slsk.StateOnline {
		return q2spot.Fail(q2spot.Offline, note(st, localIP()))
	}
	token, ok := c.RecentSearch(term, 10*time.Minute)
	if !ok {
		var err error
		if token, err = c.Search(term); err != nil {
			return q2spot.Fail(q2spot.Error, err.Error())
		}
	}
	info0, results, _ := c.Results(token, 0)
	from, _ := strconv.Atoi(cursor)
	var rows []q2spot.Row
	shown := 0
	for _, r := range results {
		if !audio[r.Ext] {
			continue
		}
		if shown++; shown <= from {
			continue
		}
		sub := r.User + " · " + quality(r) + " · " + size(int64(r.Size))
		if r.Free {
			sub += " · free"
		} else if r.Queue > 0 {
			sub += fmt.Sprintf(" · queue %d", r.Queue)
		}
		rows = append(rows, q2spot.Row{Kind: "action", URI: "download", Context: fmt.Sprintf("%d.%d", token, r.N),
			Position: "downloadfolder", Title: r.Name, Subtitle: sub})
	}
	young := time.Since(time.Unix(info0.Started, 0)) < 60*time.Second
	reply := q2spot.Reply{}
	if len(rows) > q2spot.MaxRows {
		rows = rows[:q2spot.MaxRows]
		reply.Cursor = strconv.Itoa(from + q2spot.MaxRows)
	}
	if young {
		reply.Message = refresh
		if len(rows) == 0 {
			rows = append(rows, info("Searching…", fmt.Sprintf("%d users answered", info0.Users)))
		}
	} else if len(rows) == 0 && from == 0 {
		rows = append(rows, info("Nothing found", "Try other words"))
	}
	reply.Rows = rows
	return reply
}

func (a *app) transfers(list []slsk.Info, cursor, empty, hint string) q2spot.Reply {
	from, _ := strconv.Atoi(cursor)
	reply := q2spot.Reply{Message: refresh}
	for i, t := range list {
		if i < from {
			continue
		}
		if len(reply.Rows) == q2spot.MaxRows-1 {
			reply.Cursor = strconv.Itoa(i)
			break
		}
		reply.Rows = append(reply.Rows, q2spot.Row{Kind: "list", URI: "transfer", Context: strconv.Itoa(t.ID),
			Title: t.Name, Subtitle: status(t)})
	}
	if len(list) == 0 {
		reply.Rows = append(reply.Rows, info(empty, hint))
	}
	return reply
}

func (a *app) transfer(id int) q2spot.Reply {
	var t *slsk.Info
	for _, l := range [][]slsk.Info{a.client.Downloads(), a.client.Uploads()} {
		for i := range l {
			if l[i].ID == id {
				t = &l[i]
			}
		}
	}
	if t == nil {
		return q2spot.Fail(q2spot.Unavailable, "That transfer is gone")
	}
	ctx := strconv.Itoa(id)
	rows := []q2spot.Row{info(t.Name, status(*t)), info(t.User, t.Folder)}
	act := func(op, title string) {
		rows = append(rows, q2spot.Row{Kind: "action", URI: op, Context: ctx, Title: title})
	}
	switch t.State {
	case slsk.Paused:
		act("resume", "Resume")
	case slsk.Failed:
		act("retry", "Try Again")
	case slsk.Finished:
	default:
		if !t.Up {
			act("pause", "Pause")
		}
	}
	if t.State == slsk.Finished {
		act("remove", "Remove from List")
	} else {
		act("remove", "Cancel and Remove")
	}
	return q2spot.Reply{Message: refresh, Rows: rows}
}
