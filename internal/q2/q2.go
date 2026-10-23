// Package q2 holds what the card add-ons share with Q2 Pod's side (q2-pod's patch/addon.c):
// the status file it reads, the datagram control socket it writes, CLOCK_MONOTONIC milliseconds for
// at=, and card I/O that never keeps the card busy, so stock's umount for USB storage succeeds.
package q2

import (
	"crypto/rand"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// MonoMS is CLOCK_MONOTONIC in milliseconds, the clock the payload's now_ms() reads.
func MonoMS() uint64 {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0)
	return uint64(ts.Sec)*1000 + uint64(ts.Nsec)/1e6
}

// State is a key=value status file rewritten whole (a .tmp, renamed), so a reader never sees half of
// one. Values have newlines replaced, as the payload's field() reads a line.
type State struct {
	Path string
	mu   sync.Mutex
	last string
}

// Write replaces the file with kv, in key order, when it differs from the last one written.
func (s *State) Write(kv map[string]string) error {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + strings.NewReplacer("\n", " ", "\r", " ").Replace(kv[k]) + "\n")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.String() == s.last {
		return nil
	}
	if err := WriteFile(s.Path, []byte(b.String())); err != nil {
		return err
	}
	s.last = b.String()
	return nil
}

// Remove deletes the file, as an add-on does when it exits.
func (s *State) Remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(s.Path)
	s.last = ""
}

// WriteFile writes data aside and renames it into place, synced first when it is on the card.
func WriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && OnCard(path) {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Control listens for the payload's datagrams (dgram_send: p, P, s, n, b, S<ms>, v<gain>, q) on a
// Unix datagram socket at path and hands each to fn on one goroutine, in order.
func Control(path string, fn func(cmd []byte)) (io.Closer, error) {
	os.Remove(path)
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	go func() {
		buf := make([]byte, 512)
		for {
			n, _, err := c.ReadFromUnix(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			fn(append([]byte(nil), buf[:n]...))
		}
	}()
	return c, nil
}

// Card is where the card is mounted; tests point it elsewhere.
var Card = "/mnt/mmc"

// OnCard reports whether path is under the card's mount point.
func OnCard(path string) bool {
	rel, err := filepath.Rel(Card, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// CardMounted reports whether the card is mounted: its mount point is on another device than its
// parent. While USB storage has it, /mnt/mmc is the rootfs's own empty directory.
func CardMounted() bool {
	var a, b syscall.Stat_t
	if syscall.Stat(Card, &a) != nil || syscall.Stat(filepath.Dir(Card), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}

// ErrNoCard is a card write or read while the card is away (USB storage, or no card).
var ErrNoCard = errors.New("the card is not mounted")

// Append writes data at off in path (created as needed), opened and closed around the write so the
// card is busy only for that long. The bytes are synced before it returns.
func Append(path string, off int64, data []byte) error {
	if OnCard(path) && !CardMounted() {
		return ErrNoCard
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteAt(data, off)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadAt reads len(buf) bytes (fewer at the end) at off from path, opened and closed around the read.
func ReadAt(path string, off int64, buf []byte) (int, error) {
	if OnCard(path) && !CardMounted() {
		return 0, ErrNoCard
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	n, err := f.ReadAt(buf, off)
	f.Close()
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// Logger logs to path in /tmp (never the card: an open log would keep it busy), starting the file
// again once it passes max bytes.
type Logger struct {
	Path string
	Max  int64
	mu   sync.Mutex
}

func (l *Logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.Path); err == nil && st.Size()+int64(len(p)) > l.Max {
		os.Rename(l.Path, l.Path+".1")
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return len(p), nil
	}
	f.Write(p)
	f.Close()
	return len(p), nil
}

// SetupLog sends the standard logger to /tmp/<name>.log (256 KB, then one older file).
func SetupLog(name string) {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetOutput(&Logger{Path: "/tmp/" + name + ".log", Max: 256 << 10})
}

// Every runs fn every d until stop is closed.
func Every(d time.Duration, stop <-chan struct{}, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			fn()
		}
	}
}

// LocalIP is the Q2's Wi-Fi address: the first IPv4 address of an interface that is up and not the
// loopback, wlan first; "" without one.
func LocalIP() string {
	ifs, _ := net.Interfaces()
	sort.SliceStable(ifs, func(i, j int) bool {
		return strings.HasPrefix(ifs[i].Name, "wlan") && !strings.HasPrefix(ifs[j].Name, "wlan")
	})
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, ad := range addrs {
			if n, ok := ad.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String()
			}
		}
	}
	return ""
}

// Key is n characters a person can read back: no 0/O or 1/I/L. PIN is n digits.
func Key(n int) string { return random(n, "23456789ABCDEFGHJKMNPQRSTUVWXYZ") }
func PIN(n int) string { return random(n, "0123456789") }

func random(n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}
