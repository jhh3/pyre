package tui

import "strings"

// A tiny QR encoder (ISO/IEC 18004) for short links: byte mode, error
// correction level L, versions 1-5 (single Reed-Solomon block, up to 106
// bytes). It exists so the device-login modal can show a scannable code
// without a new dependency.

var qrVersions = []struct{ data, ec, align int }{
	{19, 7, 0}, {34, 10, 18}, {55, 15, 22}, {80, 20, 26}, {108, 26, 30},
}

// qrEncode returns the module matrix (true = dark) or nil when the text is
// too long.
func qrEncode(text string) [][]bool { return qrEncodeMask(text, -1) }

// qrEncodeMask encodes with a fixed mask (0-7), or the best one for -1.
func qrEncodeMask(text string, force int) [][]bool {
	data := []byte(text)
	ver := -1
	for i, v := range qrVersions {
		if len(data)+2 <= v.data { // 4-bit mode + 8-bit count + terminator fit
			ver = i
			break
		}
	}
	if ver < 0 {
		return nil
	}
	spec := qrVersions[ver]
	// Data codewords.
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>uint(i)&1 == 1)
		}
	}
	put(0b0100, 4)
	put(len(data), 8)
	for _, b := range data {
		put(int(b), 8)
	}
	for i := 0; i < 4 && len(bits) < spec.data*8; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	cw := make([]byte, 0, spec.data+spec.ec)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				b |= 1 << uint(7-j)
			}
		}
		cw = append(cw, b)
	}
	for pad := 0; len(cw) < spec.data; pad++ {
		cw = append(cw, []byte{0xEC, 0x11}[pad%2])
	}
	cw = append(cw, rsRemainder(cw, spec.ec)...)

	size := 17 + 4*(ver+1)
	q := &qrGrid{n: size, dark: mk(size), fixed: mk(size)}
	q.finder(0, 0)
	q.finder(size-7, 0)
	q.finder(0, size-7)
	for i := 8; i < size-8; i++ {
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	if spec.align > 0 {
		a := spec.align
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				d := max(abs(dx), abs(dy))
				q.set(a+dx, a+dy, d != 1)
			}
		}
	}
	q.set(8, size-8, true) // dark module
	q.format(0)            // reserve the format areas
	// Place the codeword bits in the zigzag order.
	var stream []bool
	for _, b := range cw {
		for j := 7; j >= 0; j-- {
			stream = append(stream, b>>uint(j)&1 == 1)
		}
	}
	k := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				up := (right+1)&2 == 0
				y := vert
				if up {
					y = size - 1 - vert
				}
				if q.fixed[y][x] {
					continue
				}
				if k < len(stream) {
					q.dark[y][x] = stream[k]
				}
				k++
			}
		}
	}
	// Choose the mask with the lowest penalty.
	best, bestScore := force, -1
	for mask := 0; mask < 8 && force < 0; mask++ {
		t := q.masked(mask)
		t.format(mask)
		if s := t.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = mask, s
		}
	}
	out := q.masked(best)
	out.format(best)
	return out.dark
}

type qrGrid struct {
	n           int
	dark, fixed [][]bool
}

func mk(n int) [][]bool {
	g := make([][]bool, n)
	for i := range g {
		g[i] = make([]bool, n)
	}
	return g
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (q *qrGrid) set(x, y int, dark bool) {
	q.dark[y][x], q.fixed[y][x] = dark, true
}

func (q *qrGrid) finder(x0, y0 int) {
	for dy := -1; dy <= 7; dy++ {
		for dx := -1; dx <= 7; dx++ {
			x, y := x0+dx, y0+dy
			if x < 0 || y < 0 || x >= q.n || y >= q.n {
				continue
			}
			d := max(abs(dx-3), abs(dy-3))
			q.set(x, y, d != 2 && d != 4)
		}
	}
}

// format writes the 15-bit format information (level L) for mask.
func (q *qrGrid) format(mask int) {
	data := 0b01<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>uint(i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(q.n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, q.n-15+i, bit(i))
	}
	q.set(8, q.n-8, true)
}

func (q *qrGrid) masked(mask int) *qrGrid {
	t := &qrGrid{n: q.n, dark: mk(q.n), fixed: q.fixed}
	for y := 0; y < q.n; y++ {
		for x := 0; x < q.n; x++ {
			v := q.dark[y][x]
			if !q.fixed[y][x] && qrMask(mask, x, y) {
				v = !v
			}
			t.dark[y][x] = v
		}
	}
	return t
}

func qrMask(m, x, y int) bool {
	switch m {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	}
	return ((x+y)%2+x*y%3)%2 == 0
}

// penalty is the standard mask evaluation (rules 1-4).
func (q *qrGrid) penalty() int {
	n, s := q.n, 0
	at := func(x, y int, col bool) bool {
		if col {
			return q.dark[x][y]
		}
		return q.dark[y][x]
	}
	for _, col := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 1
			for x := 1; x <= n; x++ {
				if x < n && at(x, y, col) == at(x-1, y, col) {
					run++
					continue
				}
				if run >= 5 {
					s += run - 2
				}
				run = 1
			}
			for x := 0; x+11 <= n; x++ {
				p := [11]bool{}
				for i := range p {
					p[i] = at(x+i, y, col)
				}
				a := [11]bool{true, false, true, true, true, false, true, false, false, false, false}
				b := [11]bool{false, false, false, false, true, false, true, true, true, false, true}
				if p == a || p == b {
					s += 40
				}
			}
		}
	}
	darkN := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.dark[y][x] {
				darkN++
			}
			if x+1 < n && y+1 < n {
				c := q.dark[y][x]
				if q.dark[y][x+1] == c && q.dark[y+1][x] == c && q.dark[y+1][x+1] == c {
					s += 3
				}
			}
		}
	}
	s += abs(darkN*20-n*n*10) / (n * n) * 10
	return s
}

// rsRemainder computes Reed-Solomon error correction codewords over
// GF(256) with the QR polynomial 0x11D.
func rsRemainder(data []byte, ec int) []byte {
	gen := []byte{1}
	root := byte(1)
	for i := 0; i < ec; i++ {
		next := make([]byte, len(gen)+1)
		for j, g := range gen {
			next[j] ^= gfMul(g, root)
			next[j+1] ^= g
		}
		gen = next
		root = gfMul(root, 2)
	}
	// gen is little-endian (gen[0] constant); use the high-to-low form.
	rev := make([]byte, ec)
	for i := 0; i < ec; i++ {
		rev[i] = gen[ec-1-i]
	}
	res := make([]byte, ec)
	for _, b := range data {
		f := b ^ res[0]
		copy(res, res[1:])
		res[ec-1] = 0
		for i := 0; i < ec; i++ {
			res[i] ^= gfMul(rev[i], f)
		}
	}
	return res
}

func gfMul(a, b byte) byte {
	var r byte
	for b > 0 {
		if b&1 == 1 {
			r ^= a
		}
		hi := a & 0x80
		a <<= 1
		if hi != 0 {
			a ^= 0x1D
		}
		b >>= 1
	}
	return r
}

// qrLines renders the matrix with half blocks (two module rows per line)
// and a 2-module quiet zone. inkIsLight draws the light modules as glyph
// ink (a dark terminal: light text on a dark background); otherwise the
// dark modules are ink (a light terminal).
func qrLines(mod [][]bool, inkIsLight bool) []string {
	if mod == nil {
		return nil
	}
	const quiet = 2
	n := len(mod) + 2*quiet
	light := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= len(mod) || y >= len(mod) {
			return inkIsLight
		}
		return mod[y][x] != inkIsLight
	}
	var out []string
	for y := 0; y < n; y += 2 {
		var b strings.Builder
		for x := 0; x < n; x++ {
			top, bot := light(x, y), y+1 < n && light(x, y+1)
			switch {
			case top && bot:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bot:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		out = append(out, b.String())
	}
	return out
}
