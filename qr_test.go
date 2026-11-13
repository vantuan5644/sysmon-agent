package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// qrByteCapacity is the most bytes each version holds at level M, from the
// standard's capacity table.
var qrByteCapacity = [qrMaxVersion + 1]int{0, 14, 26, 42, 62, 84, 106, 122, 152, 180, 213}

func qrTestText(n int) string {
	const base = "https://sysmon.example:9443/#pair=123456&"
	text := base
	for i := 0; len(text) < n; i++ {
		text += string(rune('a' + i%26))
	}
	return text[:n]
}

// The worked example from the standard's annex: "HELLO WORLD" at 1-M, whose
// data codewords give these ten error correction codewords.
func TestQRReedSolomonMatchesTheStandardExample(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := qrReedSolomonRemainder(data, qrReedSolomonDivisor(10)); !bytes.Equal(got, want) {
		t.Fatalf("error correction = %v, want %v", got, want)
	}
}

func TestQRPicksTheSmallestVersionThatFits(t *testing.T) {
	for v := 1; v <= qrMaxVersion; v++ {
		if got := qrDataCodewords(v)*8 - 4 - qrCountBits(v); got/8 != qrByteCapacity[v] {
			t.Fatalf("version %d holds %d bytes, want %d", v, got/8, qrByteCapacity[v])
		}
		code, err := encodeQR(qrTestText(qrByteCapacity[v]))
		if err != nil || code.version != v {
			t.Fatalf("%d bytes: version %v (%v), want %d", qrByteCapacity[v], code, err, v)
		}
		if v < qrMaxVersion {
			if code, err := encodeQR(qrTestText(qrByteCapacity[v] + 1)); err != nil || code.version != v+1 {
				t.Fatalf("%d bytes did not move up to version %d", qrByteCapacity[v]+1, v+1)
			}
		}
	}
	if _, err := encodeQR(qrTestText(qrByteCapacity[qrMaxVersion] + 1)); !errors.Is(err, errQRTooLong) {
		t.Fatalf("an oversize text gave %v, want errQRTooLong", err)
	}
}

// Every version carries the parts a reader looks for first: three finders,
// the timing lines, the dark module, and the same valid format word in both
// copies; versions 7 and up also carry their version blocks.
func TestQRSymbolStructure(t *testing.T) {
	for v := 1; v <= qrMaxVersion; v++ {
		code, err := encodeQR(qrTestText(qrByteCapacity[v]))
		if err != nil {
			t.Fatal(err)
		}
		size := code.size
		if size != 17+4*v || len(code.dark) != size {
			t.Fatalf("version %d is %d modules, want %d", v, size, 17+4*v)
		}
		for _, corner := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
			for dy := 0; dy < 7; dy++ {
				for dx := 0; dx < 7; dx++ {
					ring := max(qrAbs(dx-3), qrAbs(dy-3))
					if want := ring != 2; code.dark[corner[1]+dy][corner[0]+dx] != want {
						t.Fatalf("version %d: finder at %v is wrong at (%d,%d)", v, corner, dx, dy)
					}
				}
			}
		}
		for i := 8; i < size-8; i++ {
			if code.dark[6][i] != (i%2 == 0) || code.dark[i][6] != (i%2 == 0) {
				t.Fatalf("version %d: timing pattern is wrong at %d", v, i)
			}
		}
		if !code.dark[size-8][8] {
			t.Fatalf("version %d: the dark module is light", v)
		}

		var first, second int
		bit := func(x, y int) int {
			if code.dark[y][x] {
				return 1
			}
			return 0
		}
		for i := 0; i <= 5; i++ {
			first |= bit(8, i) << i
		}
		first |= bit(8, 7)<<6 | bit(8, 8)<<7 | bit(7, 8)<<8
		for i := 9; i < 15; i++ {
			first |= bit(14-i, 8) << i
		}
		for i := 0; i < 8; i++ {
			second |= bit(size-1-i, 8) << i
		}
		for i := 8; i < 15; i++ {
			second |= bit(8, size-15+i) << i
		}
		if first != second {
			t.Fatalf("version %d: the format copies differ: %015b and %015b", v, first, second)
		}
		word := first ^ 0x5412
		if word>>13 != 0 {
			t.Fatalf("version %d: format names level %02b, want M (00)", v, word>>13)
		}
		rem := word >> 10
		for i := 0; i < 10; i++ {
			rem = (rem << 1) ^ ((rem >> 9) * 0x537)
		}
		if rem != word&0x3FF {
			t.Fatalf("version %d: format word %015b fails its BCH check", v, first)
		}

		if v >= 7 {
			var info, mirror int
			for i := 0; i < 18; i++ {
				a, b := size-11+i%3, i/3
				info |= bit(a, b) << i
				mirror |= bit(b, a) << i
			}
			if info != mirror || info>>12 != v {
				t.Fatalf("version %d: version blocks read %018b and %018b", v, info, mirror)
			}
		}
	}
}

func TestQRRowsAndTerminal(t *testing.T) {
	code, err := encodeQR("https://10.0.0.4:9199/#pair=370843")
	if err != nil {
		t.Fatal(err)
	}
	rows := code.rows()
	if len(rows) != code.size || rows[0][:7] != "1111111" || strings.Trim(strings.Join(rows, ""), "01") != "" {
		t.Fatalf("rows are not one string of 0 and 1 per module row: %q", rows[:1])
	}

	lines := strings.Split(strings.TrimSuffix(code.terminal(2, "  "), "\n"), "\n")
	if want := (code.size + 4 + 1) / 2; len(lines) != want {
		t.Fatalf("terminal drew %d lines, want %d", len(lines), want)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "  ") || utf8.RuneCountInString(line) != code.size+4+2 {
			t.Fatalf("line %d is %d characters wide: %q", i, utf8.RuneCountInString(line), line)
		}
	}
	// The quiet zone is printed light, and the half row past it is left to
	// the terminal's background.
	if lines[0] != "  "+strings.Repeat("█", code.size+4) {
		t.Fatalf("the top quiet zone is not solid: %q", lines[0])
	}
	if last := lines[len(lines)-1]; last != "  "+strings.Repeat("▀", code.size+4) {
		t.Fatalf("the last line is not the bottom quiet zone's top half: %q", last)
	}
}

// zbarimg reads each symbol back from a PNG. It is the check that the bits
// are in the right places, which no structural test can show; it runs where
// zbar-tools is installed (the Linux suite in WSL has it) and skips elsewhere.
func TestQRDecodesWithZbarimg(t *testing.T) {
	zbarimg, err := exec.LookPath("zbarimg")
	if err != nil {
		t.Skip("zbarimg is not installed (zbar-tools)")
	}
	texts := []string{"https://10.0.0.4:9199/#pair=370843", "https://bblwin.example-tailnet.ts.net:9443/#pair=012345"}
	for v := 1; v <= qrMaxVersion; v++ {
		texts = append(texts, qrTestText(qrByteCapacity[v]))
	}
	dir := t.TempDir()
	for i, text := range texts {
		code, err := encodeQR(text)
		if err != nil {
			t.Fatal(err)
		}
		const scale, quiet = 4, 4
		side := (code.size + 2*quiet) * scale
		img := image.NewGray(image.Rect(0, 0, side, side))
		for y := 0; y < side; y++ {
			for x := 0; x < side; x++ {
				mx, my := x/scale-quiet, y/scale-quiet
				value := uint8(255)
				if mx >= 0 && my >= 0 && mx < code.size && my < code.size && code.dark[my][mx] {
					value = 0
				}
				img.SetGray(x, y, color.Gray{Y: value})
			}
		}
		path := filepath.Join(dir, "code.png")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, img); err != nil {
			t.Fatal(err)
		}
		file.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := exec.CommandContext(ctx, zbarimg, "--quiet", "--raw", "-Sdisable", "-Sqrcode.enable", path).Output()
		cancel()
		if err != nil {
			t.Fatalf("text %d (version %d): zbarimg found no code: %v", i, code.version, err)
		}
		if got := strings.TrimRight(string(out), "\r\n"); got != text {
			t.Fatalf("text %d (version %d) read back as %q, want %q", i, code.version, got, text)
		}
	}
}
