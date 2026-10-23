package slsk

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/group-a-study/q2-soulseek/internal/q2"
)

// A shared file: its virtual path (the shared folder's name, then the path under it, joined by
// backslashes, as Soulseek clients show and request it), where it is, and what a search shows.
type shared struct {
	vpath string
	path  string
	size  int64
	ext   string
	attrs []Attr
	dir   int32 // index into Shares.dirs
}

// Shares is an index of the shared folders: every file by virtual path, every word in a path to
// the files whose path holds it, and the folders.
type Shares struct {
	files  []shared
	byPath map[string]int32
	words  map[string][]int32
	dirs   []string
	byDir  map[string][]int32
}

func (s *Shares) Files() int { return len(s.files) }
func (s *Shares) Dirs() int  { return len(s.dirs) }

// Lookup is a shared file by its virtual path.
func (s *Shares) Lookup(vpath string) (path string, size int64, ok bool) {
	if s == nil {
		return "", 0, false
	}
	i, ok := s.byPath[vpath]
	if !ok {
		return "", 0, false
	}
	return s.files[i].path, s.files[i].size, true
}

// cached is a file's attributes as last read, kept so a rescan reads only new or changed files.
type cached struct {
	Size  int64
	MTime int64
	Attrs []Attr
}

// words splits text into lower-case words of letters and digits, as both the index and a query do.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// ScanShares walks the shared folders, skipping hidden files and folders, with attributes for MP3s
// and FLACs read once and kept in cachePath. A folder named twice is shared once.
func ScanShares(roots []string, cachePath string, skip string) *Shares {
	old := map[string]cached{}
	if b, err := os.ReadFile(cachePath); err == nil {
		gob.NewDecoder(bytes.NewReader(b)).Decode(&old)
	}
	fresh := map[string]cached{}
	s := &Shares{byPath: map[string]int32{}, words: map[string][]int32{}, byDir: map[string][]int32{}}
	dirIndex := map[string]int32{}
	names := map[string]bool{}
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		name := filepath.Base(root)
		for n := 2; names[strings.ToLower(name)]; n++ {
			name = filepath.Base(root) + " " + string(rune('0'+n%10))
		}
		names[strings.ToLower(name)] = true
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			base := d.Name()
			if path != root && strings.HasPrefix(base, ".") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if skip != "" && path == skip {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			vdir := name
			if dir := filepath.Dir(rel); dir != "." {
				vdir += `\` + strings.ReplaceAll(dir, "/", `\`)
			}
			vpath := vdir + `\` + base
			c, ok := old[path]
			if !ok || c.Size != info.Size() || c.MTime != info.ModTime().Unix() {
				c = cached{Size: info.Size(), MTime: info.ModTime().Unix(), Attrs: fileAttrs(path, info.Size())}
			}
			fresh[path] = c
			di, ok := dirIndex[vdir]
			if !ok {
				di = int32(len(s.dirs))
				dirIndex[vdir] = di
				s.dirs = append(s.dirs, vdir)
			}
			i := int32(len(s.files))
			ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(base)), ".")
			s.files = append(s.files, shared{vpath: vpath, path: path, size: info.Size(), ext: ext, attrs: c.Attrs, dir: di})
			s.byPath[vpath] = i
			s.byDir[vdir] = append(s.byDir[vdir], i)
			last := ""
			ws := words(vpath)
			sort.Strings(ws)
			for _, w := range ws {
				if w != last {
					s.words[w] = append(s.words[w], i)
					last = w
				}
			}
			return nil
		})
	}
	var b bytes.Buffer
	if gob.NewEncoder(&b).Encode(fresh) == nil {
		q2.WriteFile(cachePath, b.Bytes())
	}
	return s
}

// Match finds the files whose path holds every word of the query, none of its -excluded words and
// none of the server's excluded phrases; at most max of them.
func (s *Shares) Match(query string, excluded []string, max int) []File {
	if s == nil || max <= 0 {
		return nil
	}
	var include, exclude []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		switch {
		case strings.HasPrefix(t, "-") && len(t) > 1:
			exclude = append(exclude, words(t[1:])...)
		case strings.HasPrefix(t, "*"):
			// a partial word: other words narrow it, the substring check below confirms it
			include = append(include, "*"+strings.Join(words(t[1:]), " "))
		default:
			include = append(include, words(t)...)
		}
	}
	var lists [][]int32
	var partial []string
	for _, w := range include {
		if strings.HasPrefix(w, "*") {
			if p := strings.TrimPrefix(w, "*"); p != "" {
				partial = append(partial, p)
			}
			continue
		}
		l, ok := s.words[w]
		if !ok {
			return nil
		}
		lists = append(lists, l)
	}
	if len(lists) == 0 {
		return nil // only partial words: too broad to answer
	}
	sort.Slice(lists, func(i, j int) bool { return len(lists[i]) < len(lists[j]) })
	var out []File
	for _, i := range lists[0] {
		if !all(lists[1:], i) {
			continue
		}
		f := &s.files[i]
		low := strings.ToLower(f.vpath)
		if holds(low, partial, true) && !holds(low, exclude, false) && !holds(low, excluded, false) {
			out = append(out, File{Name: f.vpath, Size: uint64(f.size), Ext: f.ext, Attrs: f.attrs})
			if len(out) == max {
				break
			}
		}
	}
	return out
}

// all reports whether i is in every sorted list.
func all(lists [][]int32, i int32) bool {
	for _, l := range lists {
		k := sort.Search(len(l), func(j int) bool { return l[j] >= i })
		if k == len(l) || l[k] != i {
			return false
		}
	}
	return true
}

// holds reports whether low holds one of subs; with every, whether it holds all of them.
func holds(low string, subs []string, every bool) bool {
	for _, s := range subs {
		if s == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(s)) != every {
			return !every
		}
	}
	return every
}

// Folder is a shared folder's files and those of its subfolders, by virtual path.
func (s *Shares) Folder(vdir string) map[string][]File {
	out := map[string][]File{}
	if s == nil {
		return out
	}
	low := strings.ToLower(vdir)
	for _, d := range s.dirs {
		ld := strings.ToLower(d)
		if ld != low && !strings.HasPrefix(ld, low+`\`) {
			continue
		}
		for _, i := range s.byDir[d] {
			f := &s.files[i]
			out[d] = append(out[d], File{Name: filepath.Base(strings.ReplaceAll(f.vpath, `\`, "/")),
				Size: uint64(f.size), Ext: f.ext, Attrs: f.attrs})
		}
	}
	return out
}

// List is every shared folder with its files (names without the folder), for a share list request.
func (s *Shares) List() ([]string, [][]File) {
	if s == nil {
		return nil, nil
	}
	dirs := append([]string(nil), s.dirs...)
	sort.Strings(dirs)
	files := make([][]File, len(dirs))
	for k, d := range dirs {
		for _, i := range s.byDir[d] {
			f := &s.files[i]
			files[k] = append(files[k], File{Name: f.vpath[len(d)+1:], Size: uint64(f.size), Ext: f.ext, Attrs: f.attrs})
		}
	}
	return dirs, files
}

// fileAttrs reads an MP3's or FLAC's bitrate, length and format from its first 64 KB.
func fileAttrs(path string, size int64) []Attr {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".mp3" && ext != ".flac" {
		return nil
	}
	buf := make([]byte, 64<<10)
	n, err := q2.ReadAt(path, 0, buf)
	if err != nil || n < 64 {
		return nil
	}
	buf = buf[:n]
	if ext == ".flac" {
		return flacAttrs(buf)
	}
	return mp3Attrs(buf, size)
}

// flacAttrs: STREAMINFO's sample rate, bits per sample and total samples, the first block after
// fLaC (an ID3v2 tag in front is skipped).
func flacAttrs(b []byte) []Attr {
	if off := id3Size(b); off > 0 && off < len(b) {
		b = b[off:]
	}
	if len(b) < 4+4+34 || string(b[:4]) != "fLaC" || b[4]&0x7f != 0 {
		return nil
	}
	si := b[8:]
	rate := uint32(si[10])<<12 | uint32(si[11])<<4 | uint32(si[12])>>4
	bits := (uint32(si[12]&1)<<4 | uint32(si[13])>>4) + 1
	total := uint64(si[13]&15)<<32 | uint64(binary.BigEndian.Uint32(si[14:18]))
	if rate == 0 {
		return nil
	}
	return []Attr{{attrDuration, uint32(total / uint64(rate))}, {attrSampleRate, rate}, {attrBitDepth, bits}}
}

// id3Size is an ID3v2 tag's length with its header, 0 without one.
func id3Size(b []byte) int {
	if len(b) < 10 || string(b[:3]) != "ID3" {
		return 0
	}
	n := int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
	if b[5]&0x10 != 0 {
		n += 10 // footer
	}
	return 10 + n
}

var mp3Bitrates = [2][16]uint32{ // MPEG-1 and MPEG-2/2.5 layer III, kbit/s
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
}
var mp3Rates = [3][3]uint32{{44100, 48000, 32000}, {22050, 24000, 16000}, {11025, 12000, 8000}}

// mp3Attrs: the first layer III frame's bitrate, or a Xing/Info or VBRI header's frame count for a
// VBR file's length and average bitrate.
func mp3Attrs(b []byte, size int64) []Attr {
	start := id3Size(b)
	if start >= len(b) {
		return nil // a tag longer than what was read: no frame to look at
	}
	for i := start; i+4 <= len(b); i++ {
		h := binary.BigEndian.Uint32(b[i:])
		if h>>21 != 0x7ff {
			continue
		}
		ver := h >> 19 & 3 // 3 MPEG-1, 2 MPEG-2, 0 MPEG-2.5
		layer := h >> 17 & 3
		bri := h >> 12 & 15
		sri := h >> 10 & 3
		if ver == 1 || layer != 1 || bri == 0 || bri == 15 || sri == 3 {
			continue
		}
		v := map[uint32]int{3: 0, 2: 1, 0: 2}[ver]
		rate := mp3Rates[v][sri]
		kbps := mp3Bitrates[min(v, 1)][bri]
		samples := uint32(1152)
		if v > 0 {
			samples = 576
		}
		mono := h>>6&3 == 3
		side := 32
		switch {
		case v == 0 && mono:
			side = 17
		case v > 0 && !mono:
			side = 17
		case v > 0 && mono:
			side = 9
		}
		if x := i + 4 + side; x+12 <= len(b) && (string(b[x:x+4]) == "Xing" || string(b[x:x+4]) == "Info") {
			flags := binary.BigEndian.Uint32(b[x+4:])
			if flags&1 != 0 {
				frames := binary.BigEndian.Uint32(b[x+8:])
				secs := uint32(uint64(frames) * uint64(samples) / uint64(rate))
				vbr := string(b[x:x+4]) == "Xing"
				avg := kbps
				if secs > 0 && vbr {
					avg = uint32(uint64(size-int64(i)) * 8 / uint64(secs) / 1000)
				}
				return []Attr{{attrBitrate, avg}, {attrDuration, secs}, {attrVBR, b2u(vbr)}}
			}
		}
		if x := i + 4 + 32; x+18 <= len(b) && string(b[x:x+4]) == "VBRI" {
			frames := binary.BigEndian.Uint32(b[x+14:])
			secs := uint32(uint64(frames) * uint64(samples) / uint64(rate))
			avg := kbps
			if secs > 0 {
				avg = uint32(uint64(size-int64(i)) * 8 / uint64(secs) / 1000)
			}
			return []Attr{{attrBitrate, avg}, {attrDuration, secs}, {attrVBR, 1}}
		}
		secs := uint32(0)
		if kbps > 0 {
			secs = uint32(uint64(size-int64(i)) * 8 / uint64(kbps) / 1000)
		}
		return []Attr{{attrBitrate, kbps}, {attrDuration, secs}, {attrVBR, 0}}
	}
	return nil
}

func b2u(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}
