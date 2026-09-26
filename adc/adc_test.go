package adc

import (
	"bytes"
	"embed"
	"errors"
	"testing"
)

// The golden pair was written by Apple's hdiutil, not by this package: a raw
// 4096-byte image was converted with `hdiutil convert -format UDCO` and the
// bytes of the single ADC run lifted out of the resulting blkx table. Nothing
// here can agree with an encoder of our own, because there is no encoder of
// our own.
//
//go:embed testdata/apple-hdiutil.adc testdata/apple-hdiutil.bin
var golden embed.FS

func TestDecompressAppleStream(t *testing.T) {
	src, err := golden.ReadFile("testdata/apple-hdiutil.adc")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want, err := golden.ReadFile("testdata/apple-hdiutil.bin")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// The fixture is worth nothing unless it exercises all three opcodes, so
	// count them before trusting what the decode proves.
	var literals, long, short int
	for i := 0; i < len(src); {
		switch op := src[i]; {
		case op&0x80 != 0:
			literals++
			i += 1 + int(op&0x7F) + 1
		case op&0x40 != 0:
			long++
			i += 3
		default:
			short++
			i += 2
		}
	}
	if literals == 0 || long == 0 || short == 0 {
		t.Fatalf("fixture exercises literals=%d long=%d short=%d: a zero means the "+
			"decode below never reaches that opcode", literals, long, short)
	}
	t.Logf("Apple stream: %d bytes, %d literal runs, %d long matches, %d short matches",
		len(src), literals, long, short)

	got, err := Decompress(src)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded %d bytes, want %d; first difference at %d",
			len(got), len(want), firstDiff(got, want))
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// Each case is a stream written by hand so that one opcode, at one boundary,
// decides the answer. The lengths and offsets are the extremes the format
// allows, which the Apple stream does not necessarily contain.
func TestDecompressOpcodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
		want []byte
	}{
		{"empty stream", nil, nil},
		{"shortest literal run", []byte{0x80, 'A'}, []byte("A")},
		{
			"longest literal run",
			append([]byte{0xFF}, bytes.Repeat([]byte("z"), 128)...),
			bytes.Repeat([]byte("z"), 128),
		},
		{
			// 0x00 -> length 3, offset byte 0 -> offset 1: repeat the last byte.
			"shortest short match overlaps itself",
			[]byte{0x80, 'A', 0x00, 0x00},
			[]byte("AAAA"),
		},
		{
			// 0x3C -> length 18, offset 2: alternates the last two bytes.
			"longest short match",
			[]byte{0x81, 'a', 'b', 0x3C, 0x01},
			[]byte("ab" + "abababababababababab"[:18]),
		},
		{
			// 0x40 -> length 4, offset 1.
			"shortest long match",
			[]byte{0x80, 'Q', 0x40, 0x00, 0x00},
			[]byte("QQQQQ"),
		},
		{
			// 0x7F -> length 67, offset 1.
			"longest long match",
			[]byte{0x80, 'w', 0x7F, 0x00, 0x00},
			bytes.Repeat([]byte("w"), 68),
		},
		{
			// A short match's offset spans ten bits, so it reaches 1024 back —
			// which takes eight literal runs to have that much output at all,
			// since one run carries at most 128 bytes.
			"short match at its furthest offset",
			append(literalRuns(1024, 'p'), 0x03, 0xFF), // length 3, offset 1024
			bytes.Repeat([]byte("p"), 1024+3),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decompress(tc.src)
			if err != nil {
				t.Fatalf("Decompress: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// literalRuns codes n copies of b as literal runs, which is the only way to
// put more than 128 bytes of arbitrary output in front of a match.
func literalRuns(n int, b byte) []byte {
	var out []byte
	for n > 0 {
		run := min(n, 128)
		out = append(out, 0x80|byte(run-1))
		out = append(out, bytes.Repeat([]byte{b}, run)...)
		n -= run
	}
	return out
}

func TestDecompressRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
		want error
	}{
		{"literal run reaches past the end", []byte{0x82, 'a', 'b'}, ErrTruncated},
		{"literal opcode with no bytes at all", []byte{0x80}, ErrTruncated},
		{"long match missing both offset bytes", []byte{0x40}, ErrTruncated},
		{"long match missing one offset byte", []byte{0x40, 0x00}, ErrTruncated},
		{"short match missing its offset byte", []byte{0x00}, ErrTruncated},
		{"short match before the start of the output", []byte{0x00, 0x00}, ErrOffset},
		{"long match before the start of the output", []byte{0x40, 0x00, 0x00}, ErrOffset},
		{
			// One byte of output, and a match asking for the two before it.
			"match one byte further back than exists",
			[]byte{0x80, 'a', 0x00, 0x01},
			ErrOffset,
		},
		{
			// 1023 bytes written and a match reaching 1024 back: the furthest
			// a short match can point, one byte short of legal.
			"short match one byte beyond the output",
			append(literalRuns(1023, 'p'), 0x03, 0xFF),
			ErrOffset,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decompress(tc.src)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("got %q alongside the error, want no output", got)
			}
		})
	}
}
