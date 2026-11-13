package main

import (
	"errors"
	"strings"
)

// qr.go draws a pairing link as a QR code, so a phone can open the dashboard
// with the PIN already filled in by pointing its camera at the host's screen.
// The agent is stdlib-only, so this is a small encoder of its own: byte mode,
// error correction level M (a code survives about 15% damage), versions 1 to
// 10, which hold up to 213 bytes. A pairing link is 35 to 70.
//
// The layout follows ISO/IEC 18004 as Project Nayuki's reference encoder sets
// it out, and qr_test.go reads the output back with an independent decoder
// (zbarimg) wherever one is installed.

// qrCode is a finished symbol: dark[y][x] is true for a dark module. It has no
// quiet zone; each renderer adds its own.
type qrCode struct {
	version int
	size    int
	dark    [][]bool
}

const qrMaxVersion = 10

// Level M, per version: error correction codewords in each block, and how
// many blocks the codewords are split into.
var (
	qrECCodewordsPerBlock = [qrMaxVersion + 1]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	qrBlockCount          = [qrMaxVersion + 1]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
	qrAlignmentCentres    = [qrMaxVersion + 1][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}
)

var errQRTooLong = errors.New("text too long for a QR code")

func encodeQR(text string) (*qrCode, error) {
	data := []byte(text)
	version := 0
	for v := 1; v <= qrMaxVersion; v++ {
		if 4+qrCountBits(v)+8*len(data) <= 8*qrDataCodewords(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, errQRTooLong
	}

	symbol := newQRSymbol(version)
	symbol.drawFunctionPatterns()
	symbol.drawCodewords(qrInterleave(qrDataSegment(data, version), version))
	best, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		symbol.applyMask(mask)
		symbol.drawFormatBits(mask)
		if penalty := symbol.penalty(); bestPenalty < 0 || penalty < bestPenalty {
			best, bestPenalty = mask, penalty
		}
		symbol.applyMask(mask) // a mask is an XOR, so applying it again undoes it
	}
	symbol.applyMask(best)
	symbol.drawFormatBits(best)
	return &qrCode{version: version, size: symbol.size, dark: symbol.dark}, nil
}

// qrCountBits is the width of the byte-mode character count.
func qrCountBits(version int) int {
	if version <= 9 {
		return 8
	}
	return 16
}

// qrRawCodewords is how many 8-bit codewords fit in the modules left after
// the function patterns (a few remainder bits stay unused).
func qrRawCodewords(version int) int {
	modules := (16*version+128)*version + 64
	if version >= 2 {
		align := version/7 + 2
		modules -= (25*align-10)*align - 55
		if version >= 7 {
			modules -= 36
		}
	}
	return modules / 8
}

func qrDataCodewords(version int) int {
	return qrRawCodewords(version) - qrECCodewordsPerBlock[version]*qrBlockCount[version]
}

type qrBits struct {
	bytes []byte
	n     int
}

func (b *qrBits) add(value, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if value>>i&1 == 1 {
			b.bytes[b.n/8] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

// qrDataSegment is the data codewords: mode, count, the bytes, a terminator,
// then the standard pad bytes up to the version's capacity.
func qrDataSegment(data []byte, version int) []byte {
	var bits qrBits
	bits.add(0b0100, 4)
	bits.add(len(data), qrCountBits(version))
	for _, b := range data {
		bits.add(int(b), 8)
	}
	capacity := 8 * qrDataCodewords(version)
	bits.add(0, min(4, capacity-bits.n))
	bits.add(0, (8-bits.n%8)%8)
	for pad := 0xEC; bits.n < capacity; pad ^= 0xEC ^ 0x11 {
		bits.add(pad, 8)
	}
	return bits.bytes
}

// qrInterleave splits the data into the version's blocks, appends each
// block's Reed-Solomon codewords, and interleaves them as the symbol stores
// them. Blocks differ in length by at most one data codeword; the shorter
// ones are listed first.
func qrInterleave(data []byte, version int) []byte {
	blocks := qrBlockCount[version]
	ecLen := qrECCodewordsPerBlock[version]
	raw := qrRawCodewords(version)
	short := blocks - raw%blocks
	shortLen := raw / blocks
	divisor := qrReedSolomonDivisor(ecLen)

	split := make([][]byte, blocks)
	offset := 0
	for i := range split {
		n := shortLen - ecLen
		if i >= short {
			n++
		}
		block := append([]byte(nil), data[offset:offset+n]...)
		offset += n
		ec := qrReedSolomonRemainder(block, divisor)
		if i < short {
			block = append(block, 0) // a placeholder, skipped below
		}
		split[i] = append(block, ec...)
	}
	out := make([]byte, 0, raw)
	for i := range split[0] {
		for j, block := range split {
			if i != shortLen-ecLen || j >= short {
				out = append(out, block[i])
			}
		}
	}
	return out
}

// qrReedSolomonDivisor is the generator polynomial of the given degree over
// GF(256) with the QR field polynomial 0x11D, highest coefficient dropped.
func qrReedSolomonDivisor(degree int) []byte {
	result := make([]byte, degree)
	result[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range result {
			result[j] = qrGFMultiply(result[j], root)
			if j+1 < len(result) {
				result[j] ^= result[j+1]
			}
		}
		root = qrGFMultiply(root, 0x02)
	}
	return result
}

func qrReedSolomonRemainder(data, divisor []byte) []byte {
	result := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for i := range result {
			result[i] ^= qrGFMultiply(divisor[i], factor)
		}
	}
	return result
}

func qrGFMultiply(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= (int(y) >> i & 1) * int(x)
	}
	return byte(z)
}

type qrSymbol struct {
	version int
	size    int
	dark    [][]bool
	fixed   [][]bool // function modules, which data and masks leave alone
}

func newQRSymbol(version int) *qrSymbol {
	size := 17 + 4*version
	symbol := &qrSymbol{version: version, size: size, dark: make([][]bool, size), fixed: make([][]bool, size)}
	for y := range symbol.dark {
		symbol.dark[y] = make([]bool, size)
		symbol.fixed[y] = make([]bool, size)
	}
	return symbol
}

func (q *qrSymbol) setFixed(x, y int, dark bool) {
	q.dark[y][x] = dark
	q.fixed[y][x] = true
}

func (q *qrSymbol) drawFunctionPatterns() {
	for i := 0; i < q.size; i++ {
		q.setFixed(6, i, i%2 == 0)
		q.setFixed(i, 6, i%2 == 0)
	}
	q.drawFinder(3, 3)
	q.drawFinder(q.size-4, 3)
	q.drawFinder(3, q.size-4)
	centres := qrAlignmentCentres[q.version]
	last := len(centres) - 1
	for i, cy := range centres {
		for j, cx := range centres {
			if i == 0 && j == 0 || i == 0 && j == last || i == last && j == 0 {
				continue // a finder sits there
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.setFixed(cx+dx, cy+dy, max(qrAbs(dx), qrAbs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormatBits(0) // reserves the area; the chosen mask's bits replace these
	q.drawVersion()
}

// drawFinder draws a finder pattern centred on (x, y) with its light
// separator, clipped at the symbol's edge.
func (q *qrSymbol) drawFinder(x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			xx, yy := x+dx, y+dy
			if xx < 0 || xx >= q.size || yy < 0 || yy >= q.size {
				continue
			}
			distance := max(qrAbs(dx), qrAbs(dy))
			q.setFixed(xx, yy, distance != 2 && distance != 4)
		}
	}
}

// drawFormatBits writes level M and the mask, BCH-protected, in both copies.
func (q *qrSymbol) drawFormatBits(mask int) {
	data := 0<<3 | mask // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }

	for i := 0; i <= 5; i++ {
		q.setFixed(8, i, bit(i))
	}
	q.setFixed(8, 7, bit(6))
	q.setFixed(8, 8, bit(7))
	q.setFixed(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.setFixed(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.setFixed(q.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.setFixed(8, q.size-15+i, bit(i))
	}
	q.setFixed(8, q.size-8, true) // the module that is always dark
}

// drawVersion writes the two version blocks that versions 7 and up carry.
func (q *qrSymbol) drawVersion() {
	if q.version < 7 {
		return
	}
	rem := q.version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := q.version<<12 | rem
	for i := 0; i < 18; i++ {
		dark := bits>>i&1 == 1
		a, b := q.size-11+i%3, i/3
		q.setFixed(a, b, dark)
		q.setFixed(b, a, dark)
	}
}

// drawCodewords places the codewords in the zigzag order: two-module columns
// from the right edge, alternately upward and downward, skipping the vertical
// timing column.
func (q *qrSymbol) drawCodewords(codewords []byte) {
	i := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.size - 1 - vert
				}
				if !q.fixed[y][x] && i < len(codewords)*8 {
					q.dark[y][x] = codewords[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrSymbol) applyMask(mask int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.fixed[y][x] {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (x/3+y/2)%2 == 0
			case 5:
				invert = x*y%2+x*y%3 == 0
			case 6:
				invert = (x*y%2+x*y%3)%2 == 0
			case 7:
				invert = ((x+y)%2+x*y%3)%2 == 0
			}
			if invert {
				q.dark[y][x] = !q.dark[y][x]
			}
		}
	}
}

// penalty scores a masked symbol by the standard's four rules (lower is
// better): long runs, 2x2 blocks, finder-like patterns, and dark/light balance.
func (q *qrSymbol) penalty() int {
	const n1, n2, n3, n4 = 3, 3, 40, 10
	result := 0
	for _, column := range []bool{false, true} {
		for a := 0; a < q.size; a++ {
			runDark := false
			run := 0
			var history [7]int
			for b := 0; b < q.size; b++ {
				dark := q.dark[a][b]
				if column {
					dark = q.dark[b][a]
				}
				if dark == runDark {
					run++
					if run == 5 {
						result += n1
					} else if run > 5 {
						result++
					}
					continue
				}
				q.addRunHistory(run, &history)
				if !runDark {
					result += qrFinderPatterns(&history) * n3
				}
				runDark = dark
				run = 1
			}
			if runDark {
				q.addRunHistory(run, &history)
				run = 0
			}
			q.addRunHistory(run+q.size, &history) // the light border after the last run
			result += qrFinderPatterns(&history) * n3
		}
	}
	dark := 0
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.dark[y][x] {
				dark++
			}
			if x+1 < q.size && y+1 < q.size {
				c := q.dark[y][x]
				if c == q.dark[y][x+1] && c == q.dark[y+1][x] && c == q.dark[y+1][x+1] {
					result += n2
				}
			}
		}
	}
	total := q.size * q.size
	result += ((qrAbs(dark*20-total*10)+total-1)/total - 1) * n4
	return result
}

func (q *qrSymbol) addRunHistory(run int, history *[7]int) {
	if history[0] == 0 {
		run += q.size // the light border before the first run
	}
	copy(history[1:], history[:6])
	history[0] = run
}

// qrFinderPatterns counts 1:1:3:1:1 runs with light space on one side.
func qrFinderPatterns(history *[7]int) int {
	n := history[1]
	core := n > 0 && history[2] == n && history[3] == n*3 && history[4] == n && history[5] == n
	count := 0
	if core && history[0] >= n*4 && history[6] >= n {
		count++
	}
	if core && history[6] >= n*4 && history[0] >= n {
		count++
	}
	return count
}

func qrAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// rows is the symbol as one string per module row, '1' for dark, which is
// how /api/pair/window hands it to the dashboard to draw.
func (c *qrCode) rows() []string {
	rows := make([]string, c.size)
	for y, row := range c.dark {
		line := make([]byte, c.size)
		for x, dark := range row {
			line[x] = '0'
			if dark {
				line[x] = '1'
			}
		}
		rows[y] = string(line)
	}
	return rows
}

// terminal draws the code with half-block characters, two module rows to a
// line, inside a quiet zone of the given width and indented by indent spaces.
// The light modules are the ones printed, so on the dark background most
// terminals have the code reads dark on light, the way a camera expects it.
func (c *qrCode) terminal(quiet int, indent string) string {
	light := func(x, y int) bool {
		if x < -quiet || x >= c.size+quiet || y < -quiet || y >= c.size+quiet {
			return false // past the quiet zone: the terminal's own background
		}
		if x < 0 || x >= c.size || y < 0 || y >= c.size {
			return true
		}
		return !c.dark[y][x]
	}
	var b strings.Builder
	for y := -quiet; y < c.size+quiet; y += 2 {
		b.WriteString(indent)
		for x := -quiet; x < c.size+quiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
