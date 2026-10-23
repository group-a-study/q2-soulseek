package slsk

import (
	"log"
	"net"
	"time"

	"github.com/group-a-study/q2-soulseek/internal/power"
)

// The distributed network carries other users' searches to us. We join it only while sharing is
// active, as a leaf: one parent, no children (AcceptChildren false).

// distributedAnnounce tells the server whether we want a parent: while sharing is active we have
// none yet (so it sends PossibleParents); otherwise we say we have one, so it sends none and never
// makes us a branch root.
func (c *Client) distributedAnnounce() {
	c.mu.Lock()
	active, have := c.shareActive, c.parent != nil
	c.mu.Unlock()
	c.serverSend(coded(sAcceptChildren).boolean(false))
	c.serverSend(coded(sHaveNoParent).boolean(active && !have))
}

// findParent tries the server's candidates in turn, directly, while sharing is active and we have
// no parent; the first that connects and sends us a search becomes it.
func (c *Client) findParent(list []peerCandidate) {
	c.mu.Lock()
	if !c.shareActive || c.parent != nil || c.findingP {
		c.mu.Unlock()
		return
	}
	c.findingP = true
	me := c.cfg.Username
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.findingP = false
		c.mu.Unlock()
	}()
	for _, cand := range list {
		c.mu.Lock()
		stop := !c.shareActive || c.parent != nil
		c.mu.Unlock()
		if stop {
			return
		}
		nc, err := net.DialTimeout("tcp4", peerAddr{cand.ip, cand.port}.String(), 10*time.Second)
		if err != nil {
			continue
		}
		nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := nc.Write(coded8(iPeerInit).str(me).str("D").u32(0).frame()); err != nil {
			nc.Close()
			continue
		}
		nc.SetWriteDeadline(time.Time{})
		p := newConn(c, nc, 'D', cand.user)
		if c.serveParent(p) {
			return
		}
	}
}

// serveParent reads a candidate's distributed messages; it becomes our parent with its first search.
// It reports whether it was adopted (and has since gone).
func (c *Client) serveParent(p *conn) bool {
	defer p.close()
	adopted := false
	level, root := uint32(0), p.user
	deadline := time.Now().Add(60 * time.Second) // a candidate that sends no search in time is dropped
	for {
		if !adopted {
			p.nc.SetReadDeadline(deadline)
		} else {
			p.nc.SetReadDeadline(time.Now().Add(10 * time.Minute))
		}
		body, err := readFrame(p.nc, 1<<20)
		if err != nil || len(body) < 1 {
			break
		}
		r := &reader{b: body[1:]}
		switch body[0] {
		case dBranchLevel:
			level = r.u32()
			if level == 0 {
				root = p.user
			}
		case dBranchRoot:
			if s := r.str(); r.err == nil {
				root = s
			}
		case dSearch, dEmbedded:
			if body[0] == dEmbedded && r.u8() != dSearch {
				continue
			}
			if !adopted {
				c.mu.Lock()
				ok := c.parent == nil && c.shareActive
				if ok {
					c.parent, c.parentLvl, c.rootName = p, level, root
					c.notify()
				}
				c.mu.Unlock()
				if !ok {
					return false
				}
				adopted = true
				log.Printf("distributed parent %s (level %d, root %s)", Text(p.user), level, Text(root))
				c.serverSend(coded(sHaveNoParent).boolean(false))
				c.serverSend(coded(sBranchRoot).str(root))
				c.serverSend(coded(sBranchLevel).u32(level + 1))
			}
			c.distribSearch(p, r)
		}
	}
	if adopted {
		c.mu.Lock()
		if c.parent == p {
			c.parent = nil
		}
		active := c.shareActive
		c.notify()
		c.mu.Unlock()
		if active {
			c.serverSend(coded(sHaveNoParent).boolean(true))
		}
	}
	return adopted
}

// distribSearch is a search from our parent, or (from nil) one the server embedded because we are
// a branch root.
func (c *Client) distribSearch(from *conn, r *reader) {
	if r.u32() != 49 {
		return
	}
	user, token, query := r.str(), r.u32(), r.str()
	if r.err == nil {
		c.answer(user, token, query)
	}
}

// answer answers a search on its own goroutine, unless as many are being answered as answers holds.
func (c *Client) answer(user string, token uint32, query string) {
	select {
	case c.answers <- struct{}{}:
		go func() {
			defer func() { <-c.answers }()
			c.searchRequest(user, token, query)
		}()
	default: // busy answering: this one goes unanswered
	}
}

// powerLoop follows the cable and the battery every 10 s, sharing only as the setting allows.
func (c *Client) powerLoop() {
	c.updateSharing()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.updateSharing()
		}
	}
}

// ShareAllowed is whether the sharing setting lets us share now, on this power.
func ShareAllowed(mode string, min int, p power.Status) bool {
	switch mode {
	case ShareCharging:
		return p.External
	case ShareBattery:
		return p.External || p.Capacity >= min
	}
	return false
}

// updateSharing starts or stops sharing as the power and the setting allow. Stopping ends running
// uploads (they wait in the queue for the next time), leaves the distributed network and stops
// answering searches.
func (c *Client) updateSharing() {
	p := power.Read()
	c.mu.Lock()
	active := ShareAllowed(c.cfg.Sharing, c.cfg.BatteryMin, p) && c.shares != nil
	changed := active != c.shareActive
	c.pow, c.shareActive = p, active
	var parent *conn
	if changed && !active {
		parent = c.parent
		for _, u := range c.ups {
			if u.State == Requesting || u.State == Starting || u.State == Uploading {
				if u.conn != nil {
					u.conn.Close()
				}
				u.State, u.Err = Queued, "Waiting: sharing paused"
			}
		}
	}
	if changed {
		c.notify()
	}
	c.mu.Unlock()
	if parent != nil {
		parent.close()
	}
	if changed {
		log.Printf("sharing %v (power %+v)", active, p)
		c.distributedAnnounce()
	}
}
