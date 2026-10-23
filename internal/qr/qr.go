// Package qr encodes a QR code (ISO/IEC 18004) in byte mode at error correction level M, the
// smallest version that holds the data, with the mask of least penalty. It follows the structure of
// Project Nayuki's QR Code generator (MIT), reduced to what the Soulseek add-on needs: a short URL
// for the Q2's screen.
package qr

import "errors"

var ErrTooLong = errors.New("qr: data too long")

// Level M's error correction codewords per block and block count, by version (index 0 unused).
var eccPerBlock = [41]int{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26,
	26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28}
var eccBlocks = [41]int{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18,
	20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49}

const formatM = 0 // level M's two format bits

// Code is a square of modules, true dark: Modules[y][x].
type Code struct {
	Version int
	Size    int
	Mask    int
	Modules [][]bool
	fn      [][]bool // function modules, which data and masks leave alone
}

func rawModules(ver int) int {
	n := (16*ver+128)*ver + 64
	if ver >= 2 {
		align := ver/7 + 2
		n -= (25*align-10)*align - 55
		if ver >= 7 {
			n -= 36
		}
	}
	return n
}

func dataCodewords(ver int) int { return rawModules(ver)/8 - eccPerBlock[ver]*eccBlocks[ver] }

// Encode makes the code for data, choosing the mask by penalty.
func Encode(data []byte) (*Code, error) { return EncodeMask(data, -1) }

// EncodeMask is Encode with a fixed mask 0-7, or -1 to choose.
func EncodeMask(data []byte, mask int) (*Code, error) {
	ver := 1
	for ; ver <= 40; ver++ {
		count := 8
		if ver >= 10 {
			count = 16
		}
		if 4+count+8*len(data) <= dataCodewords(ver)*8 {
			break
		}
	}
	if ver > 40 {
		return nil, ErrTooLong
	}
	// Mode, count, data, terminator, byte padding and pad codewords.
	var bits []bool
	put := func(v uint, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>uint(i)&1 != 0)
		}
	}
	put(4, 4)
	if ver >= 10 {
		put(uint(len(data)), 16)
	} else {
		put(uint(len(data)), 8)
	}
	for _, b := range data {
		put(uint(b), 8)
	}
	capBits := dataCodewords(ver) * 8
	for i := 0; i < 4 && len(bits) < capBits; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	for pad := uint(0xec); len(bits) < capBits; pad ^= 0xec ^ 0x11 {
		put(pad, 8)
	}
	words := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			words[i>>3] |= 1 << (7 - uint(i&7))
		}
	}
	c := &Code{Version: ver, Size: ver*4 + 17}
	c.Modules = grid(c.Size)
	c.fn = grid(c.Size)
	c.functionPatterns()
	c.codewords(interleave(ver, words))
	if mask < 0 {
		best := 1 << 30
		for m := 0; m < 8; m++ {
			c.applyMask(m)
			c.formatBits(m)
			if p := c.penalty(); p < best {
				best, mask = p, m
			}
			c.applyMask(m) // undo: XOR
		}
	}
	c.Mask = mask
	c.applyMask(mask)
	c.formatBits(mask)
	c.fn = nil
	return c, nil
}

func grid(n int) [][]bool {
	g := make([][]bool, n)
	for i := range g {
		g[i] = make([]bool, n)
	}
	return g
}

func (c *Code) set(x, y int, dark bool) {
	c.Modules[y][x] = dark
	c.fn[y][x] = true
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (c *Code) functionPatterns() {
	for i := 0; i < c.Size; i++ {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	for _, p := range [][2]int{{3, 3}, {c.Size - 4, 3}, {3, c.Size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := p[0]+dx, p[1]+dy
				if x >= 0 && x < c.Size && y >= 0 && y < c.Size {
					d := max(abs(dx), abs(dy))
					c.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	pos := alignments(c.Version)
	for i := range pos {
		for j := range pos {
			if i == 0 && j == 0 || i == 0 && j == len(pos)-1 || i == len(pos)-1 && j == 0 {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.set(pos[i]+dx, pos[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.formatBits(0) // reserved now, drawn for real once the mask is known
	if c.Version >= 7 {
		rem := c.Version
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*0x1f25
		}
		v := c.Version<<12 | rem
		for i := 0; i < 18; i++ {
			a, b := c.Size-11+i%3, i/3
			c.set(a, b, v>>uint(i)&1 != 0)
			c.set(b, a, v>>uint(i)&1 != 0)
		}
	}
}

func alignments(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := (ver*8 + n*3 + 5) / (n*4 - 4) * 2
	pos := make([]int, n)
	pos[0] = 6
	for i, p := n-1, ver*4+17-7; i >= 1; i, p = i-1, p-step {
		pos[i] = p
	}
	return pos
}

func (c *Code) formatBits(mask int) {
	data := formatM<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>uint(i)&1 != 0 }
	for i := 0; i <= 5; i++ {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		c.set(c.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, c.Size-15+i, bit(i))
	}
	c.set(8, c.Size-8, true)
}

// interleave splits the data codewords into the version's blocks, appends each block's error
// correction and interleaves them.
func interleave(ver int, data []byte) []byte {
	blocks, ecc := eccBlocks[ver], eccPerBlock[ver]
	raw := rawModules(ver) / 8
	short := blocks - raw%blocks
	shortLen := raw / blocks
	div := rsDivisor(ecc)
	var all [][]byte
	for i, k := 0, 0; i < blocks; i++ {
		n := shortLen - ecc
		if i >= short {
			n++
		}
		dat := append([]byte(nil), data[k:k+n]...)
		k += n
		e := rsRemainder(dat, div)
		if i < short {
			dat = append(dat, 0) // a placeholder, skipped below, so every block is as long
		}
		all = append(all, append(dat, e...))
	}
	var out []byte
	for i := range all[0] {
		for j, b := range all {
			if i != shortLen-ecc || j >= short {
				out = append(out, b[i])
			}
		}
	}
	return out
}

func gfMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x11d
		z ^= int(y>>uint(i)&1) * int(x)
	}
	return byte(z)
}

func rsDivisor(degree int) []byte {
	r := make([]byte, degree)
	r[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range r {
			r[j] = gfMul(r[j], root)
			if j+1 < len(r) {
				r[j] ^= r[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return r
}

func rsRemainder(data, div []byte) []byte {
	r := make([]byte, len(div))
	for _, b := range data {
		f := b ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i, d := range div {
			r[i] ^= gfMul(d, f)
		}
	}
	return r
}

func (c *Code) codewords(data []byte) {
	i := 0
	for right := c.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < c.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = c.Size - 1 - vert
				}
				if !c.fn[y][x] && i < len(data)*8 {
					c.Modules[y][x] = data[i>>3]>>(7-uint(i&7))&1 != 0
					i++
				}
			}
		}
	}
}

func (c *Code) applyMask(m int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !c.fn[y][x] {
				c.Modules[y][x] = !c.Modules[y][x]
			}
		}
	}
}

func (c *Code) penalty() int {
	const n1, n2, n3, n4 = 3, 3, 40, 10
	p := 0
	line := func(get func(i int) bool) {
		color, run := false, 0
		var hist [7]int
		for i := 0; i < c.Size; i++ {
			if get(i) == color {
				run++
				if run == 5 {
					p += n1
				} else if run > 5 {
					p++
				}
			} else {
				c.addHistory(run, &hist)
				if !color {
					p += countFinder(&hist) * n3
				}
				color, run = get(i), 1
			}
		}
		if color {
			c.addHistory(run, &hist)
			run = 0
		}
		c.addHistory(run+c.Size, &hist)
		p += countFinder(&hist) * n3
	}
	for y := 0; y < c.Size; y++ {
		line(func(x int) bool { return c.Modules[y][x] })
	}
	for x := 0; x < c.Size; x++ {
		line(func(y int) bool { return c.Modules[y][x] })
	}
	dark := 0
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			v := c.Modules[y][x]
			if v {
				dark++
			}
			if x < c.Size-1 && y < c.Size-1 && v == c.Modules[y][x+1] && v == c.Modules[y+1][x] &&
				v == c.Modules[y+1][x+1] {
				p += n2
			}
		}
	}
	total := c.Size * c.Size
	k := (abs(dark*20-total*10)+total-1)/total - 1
	return p + k*n4
}

func (c *Code) addHistory(run int, h *[7]int) {
	if h[0] == 0 {
		run += c.Size // the light border before the first run
	}
	copy(h[1:], h[:6])
	h[0] = run
}

func countFinder(h *[7]int) int {
	n := h[1]
	core := n > 0 && h[2] == n && h[3] == n*3 && h[4] == n && h[5] == n
	k := 0
	if core && h[0] >= n*4 && h[6] >= n {
		k++
	}
	if core && h[6] >= n*4 && h[0] >= n {
		k++
	}
	return k
}

// Hex is the code as the payload reads it (patch/soulseek.c): each row as ceil(size/4) hex digits,
// the leftmost module the high bit of the first.
func (c *Code) Hex() string {
	w := (c.Size + 3) / 4
	out := make([]byte, 0, w*c.Size)
	for _, row := range c.Modules {
		for d := 0; d < w; d++ {
			v := 0
			for b := 0; b < 4; b++ {
				if x := d*4 + b; x < c.Size && row[x] {
					v |= 8 >> uint(b)
				}
			}
			out = append(out, "0123456789abcdef"[v])
		}
	}
	return string(out)
}
