package qr

import "testing"

// The golden code was decoded by ZXing (zxing-cpp 2.x), as were all eight masks of it, and again
// with a 3x3 patch of its data flipped, which only correct error correction survives.
const golden = "fe7dd3f8822d2208ba9922e8bafa6ae8bad592e8829da208feaaabf80082a000be2b7be0b83df288e293418025a0969037937d6054f5d2a86aebce20d4eb95106ae94c20bccdf6e886476260a51596909be36fb800c9d8f8fe099ae082fa0890baf10ff8badbad18bac53ad082520d90fecb0060"

func TestGolden(t *testing.T) {
	c, err := Encode([]byte("http://192.168.1.23:8080/?k=ABCDEFGHJKLM"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 3 || c.Size != 29 || c.Mask != 2 || c.Hex() != golden {
		t.Fatalf("v%d size %d mask %d\n%s", c.Version, c.Size, c.Mask, c.Hex())
	}
}

func TestVersions(t *testing.T) {
	for n, want := range map[int]int{14: 1, 15: 2, 26: 2, 27: 3, 42: 3, 43: 4, 62: 4, 63: 5, 213: 10, 2331: 40} {
		c, err := Encode(make([]byte, n))
		if err != nil || c.Version != want || c.Size != want*4+17 || len(c.Modules) != c.Size {
			t.Errorf("%d bytes: version %v, want %d (%v)", n, c, want, err)
		}
	}
	if _, err := Encode(make([]byte, 2332)); err != ErrTooLong {
		t.Errorf("2332 bytes: %v", err)
	}
}

func TestFinders(t *testing.T) {
	c, _ := Encode([]byte("x"))
	// A finder's corners are dark, its separator light, the dark module at (8, size-8) set.
	for _, p := range [][2]int{{0, 0}, {6, 0}, {0, 6}, {c.Size - 1, 0}, {0, c.Size - 1}} {
		if !c.Modules[p[1]][p[0]] {
			t.Errorf("module %v light", p)
		}
	}
	if c.Modules[7][7] || !c.Modules[c.Size-8][8] {
		t.Error("separator or dark module")
	}
}
