// Package slsk is a Soulseek client: the server connection, peer, file and distributed connections,
// searches, downloads, and uploads of the shared folders (contrib/shanlingq2/README). Message layouts follow
// Nicotine+'s protocol documentation (doc/SLSKPROTOCOL.md).
package slsk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"unicode/utf8"
)

// Server message codes (uint32).
const (
	sLogin             = 1
	sSetWaitPort       = 2
	sGetPeerAddress    = 3
	sConnectToPeer     = 18
	sMessageUser       = 22
	sMessageAcked      = 23
	sFileSearch        = 26
	sSetStatus         = 28
	sSharedFoldersFile = 35
	sRelogged          = 41
	sHaveNoParent      = 71
	sEmbeddedMessage   = 93
	sAcceptChildren    = 100
	sPossibleParents   = 102
	sBranchLevel       = 126
	sBranchRoot        = 127
	sResetDistributed  = 130
	sExcludedPhrases   = 160
	sCantConnectToPeer = 1001
)

// Peer message codes (uint32).
const (
	pShareListRequest  = 4
	pShareListResponse = 5
	pSearchResponse    = 9
	pUserInfoRequest   = 15
	pUserInfoResponse  = 16
	pFolderRequest     = 36
	pFolderResponse    = 37
	pTransferRequest   = 40
	pTransferResponse  = 41
	pQueueUpload       = 43
	pPlaceInQueue      = 44
	pUploadFailed      = 46
	pUploadDenied      = 50
	pPlaceInQueueReq   = 51
)

// Distributed message codes (uint8).
const (
	dSearch      = 3
	dBranchLevel = 4
	dBranchRoot  = 5
	dEmbedded    = 93
)

// Init message codes (uint8).
const (
	iPierceFirewall = 0
	iPeerInit       = 1
)

// File attribute codes.
const (
	attrBitrate    = 0
	attrDuration   = 1
	attrVBR        = 2
	attrSampleRate = 4
	attrBitDepth   = 5
)

const (
	maxServerMessage = 8 << 20
	maxPeerMessage   = 16 << 20
)

var errShort = errors.New("slsk: short message")

// msg builds a message body.
type msg struct{ b []byte }

func (m *msg) u8(v uint8) *msg   { m.b = append(m.b, v); return m }
func (m *msg) u32(v uint32) *msg { m.b = binary.LittleEndian.AppendUint32(m.b, v); return m }
func (m *msg) u64(v uint64) *msg { m.b = binary.LittleEndian.AppendUint64(m.b, v); return m }
func (m *msg) boolean(v bool) *msg {
	if v {
		return m.u8(1)
	}
	return m.u8(0)
}
func (m *msg) str(s string) *msg { m.u32(uint32(len(s))); m.b = append(m.b, s...); return m }

// frame is the body with its uint32 length in front.
func (m *msg) frame() []byte {
	out := make([]byte, 4, 4+len(m.b))
	binary.LittleEndian.PutUint32(out, uint32(len(m.b)))
	return append(out, m.b...)
}

// coded starts a message with a uint32 code (server and peer messages).
func coded(code uint32) *msg { return (&msg{}).u32(code) }

// coded8 starts a message with a uint8 code (init and distributed messages).
func coded8(code uint8) *msg { return (&msg{}).u8(code) }

// reader takes a message body apart; the first error sticks and later reads return zero values.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.err = errShort
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() uint8 {
	if v := r.take(1); v != nil {
		return v[0]
	}
	return 0
}
func (r *reader) u16() uint16 {
	if v := r.take(2); v != nil {
		return binary.LittleEndian.Uint16(v)
	}
	return 0
}
func (r *reader) u32() uint32 {
	if v := r.take(4); v != nil {
		return binary.LittleEndian.Uint32(v)
	}
	return 0
}
func (r *reader) u64() uint64 {
	if v := r.take(8); v != nil {
		return binary.LittleEndian.Uint64(v)
	}
	return 0
}
func (r *reader) boolean() bool { return r.u8() != 0 }
func (r *reader) str() string {
	n := r.u32()
	if r.err == nil && int(n) > len(r.b) {
		r.err = errShort
		return ""
	}
	return string(r.take(int(n)))
}

// readFrame reads one length-prefixed message body (code included) of at most max bytes.
func readFrame(rd io.Reader, max int) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(rd, h[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(h[:])
	if int64(n) > int64(max) {
		return nil, errors.New("slsk: message too large")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(rd, b)
	return b, err
}

func compress(b []byte) []byte {
	var out bytes.Buffer
	w, _ := zlib.NewWriterLevel(&out, zlib.DefaultCompression)
	w.Write(b)
	w.Close()
	return out.Bytes()
}

// decompress inflates at most max bytes.
func decompress(b []byte, max int64) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, max+1))
	if err == nil && int64(len(out)) > max {
		err = errors.New("slsk: inflated message too large")
	}
	return out, err
}

// Text shows a protocol string: UTF-8 as is, anything else read as Latin-1, as Nicotine+ does.
// Names are kept as their original bytes elsewhere, so a request sends back exactly what a peer sent.
func Text(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	r := make([]rune, len(s))
	for i := 0; i < len(s); i++ {
		r[i] = rune(s[i])
	}
	return string(r)
}

// Attr is a file attribute: code and value.
type Attr struct{ Code, Value uint32 }

// File is one entry of a search result, folder or share list.
type File struct {
	Name  string // the full virtual path, backslash separated, as the peer sent it
	Size  uint64
	Ext   string
	Attrs []Attr
}

func (f File) attr(code uint32) (uint32, bool) {
	for _, a := range f.Attrs {
		if a.Code == code {
			return a.Value, true
		}
	}
	return 0, false
}

func readFiles(r *reader, max int) []File {
	n := int(r.u32())
	if r.err != nil || n > max {
		r.err = errShort
		return nil
	}
	files := make([]File, 0, min(n, 1024))
	for i := 0; i < n && r.err == nil; i++ {
		r.u8()
		f := File{Name: r.str(), Size: r.u64(), Ext: r.str()}
		na := int(r.u32())
		if na > 64 {
			r.err = errShort
			break
		}
		for j := 0; j < na; j++ {
			f.Attrs = append(f.Attrs, Attr{r.u32(), r.u32()})
		}
		files = append(files, f)
	}
	return files
}

func writeFiles(m *msg, files []File) {
	m.u32(uint32(len(files)))
	for _, f := range files {
		m.u8(1).str(f.Name).u64(f.Size).str(f.Ext).u32(uint32(len(f.Attrs)))
		for _, a := range f.Attrs {
			m.u32(a.Code).u32(a.Value)
		}
	}
}
