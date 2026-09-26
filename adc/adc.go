// Package adc decodes Apple Data Compression (ADC), the byte-oriented LZ77
// variant Apple uses for the "UDCO" flavour of UDIF disk images and, before
// that, for resources in classic Mac OS.
//
// The format has no header, no checksum and no stored output length: a stream
// is a bare sequence of three opcodes, and it ends when its bytes run out. A
// caller that knows how long the output should be must check that itself.
//
//	out, err := adc.Decompress(src)
//
// CGO is not used, so this builds for every target Go builds for.
//
// # The opcodes
//
// The first byte of each opcode says which one it is, and the format was read
// off streams written by Apple's own hdiutil rather than from any
// implementation of it:
//
//	1xxxxxxx                    literal run, (x+1) bytes follow, 1..128
//	01xxxxxx hhhhhhhh llllllll  match, length (x+4) 4..67,  offset (hl+1) 1..65536
//	00xxxxyy yyyyyyyy           match, length (x+3) 3..18,  offset (y+1)  1..1024
//
// An offset counts backwards from the end of the output produced so far, so 1
// is the byte just written. A match may be longer than its offset, in which
// case it repeats what it has already copied; that is the usual way a run of
// one byte is coded, and it is why the copy runs one byte at a time.
package adc

import "errors"

// ErrTruncated is returned when an opcode's operands, or a literal run's
// bytes, reach past the end of the input.
var ErrTruncated = errors.New("adc: truncated stream")

// ErrOffset is returned when a match points further back than the output
// written so far, which no stream produced by a correct encoder ever does.
var ErrOffset = errors.New("adc: match offset reaches before the start of the output")

// Decompress returns the bytes the stream encodes. An empty input gives an
// empty output and no error: the format ends by exhaustion, so a stream of no
// opcodes is a stream of no bytes.
func Decompress(src []byte) ([]byte, error) {
	// The ratio is a guess, not a bound: the loop grows out as it needs to.
	// Apple's own streams run about 3:1 on the images this was measured on.
	out := make([]byte, 0, len(src)*3)
	for i := 0; i < len(src); {
		op := src[i]
		switch {
		case op&0x80 != 0: // literal run
			n := int(op&0x7F) + 1
			i++
			if i+n > len(src) {
				return nil, ErrTruncated
			}
			out = append(out, src[i:i+n]...)
			i += n

		case op&0x40 != 0: // long match, two offset bytes
			if i+3 > len(src) {
				return nil, ErrTruncated
			}
			n := int(op&0x3F) + 4
			off := (int(src[i+1])<<8 | int(src[i+2])) + 1
			i += 3
			var err error
			if out, err = copyMatch(out, off, n); err != nil {
				return nil, err
			}

		default: // short match, offset's low byte follows
			if i+2 > len(src) {
				return nil, ErrTruncated
			}
			n := int(op&0x3C)>>2 + 3
			off := (int(op&0x03)<<8 | int(src[i+1])) + 1
			i += 2
			var err error
			if out, err = copyMatch(out, off, n); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// copyMatch appends n bytes taken off bytes back from the end of out. It runs
// one byte at a time because a match is allowed to overlap what it copies.
func copyMatch(out []byte, off, n int) ([]byte, error) {
	if off > len(out) {
		return nil, ErrOffset
	}
	start := len(out) - off
	for j := 0; j < n; j++ {
		out = append(out, out[start+j])
	}
	return out, nil
}
