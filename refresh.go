package dmg

// RefreshChecksums: the writer's side of the checks readAllUDIFSectors makes.
//
// v0.3.0 started verifying the blkx checksum, and that left no way for a writer
// that edits an image IN PLACE to satisfy it. Such a writer changes bytes inside
// the data fork and cannot then recompute the three declared values without
// re-implementing this package's rules -- including the one that costs the most
// to get wrong, that a blkx checksum skips the runs which only zero-fill. Two
// implementations of one rule is how two definitions start to drift, so the rule
// stays here and the fix-up is exported instead.

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"unicode"
)

// RefreshChecksums recomputes the checksums of the UDIF image at path and
// writes them back: each blkx table's CRC-32, the koly's master checksum over
// those, and the koly's data-fork checksum.
//
// It is for a writer that has modified an image in place -- the sector bytes are
// read back from the image itself, so whatever is in the data fork now is what
// the new values describe. It is NOT a repair: an image whose data fork was
// damaged rather than edited comes out of this consistent and wrong, which is
// the one thing a checksum is there to prevent. Call it because you changed the
// bytes, never because a check failed.
//
// The XML plist is edited rather than regenerated. Only the base64 inside each
// blkx <data> element is rewritten, and the whitespace inside it is preserved
// character for character, so the plist keeps its length and every key this
// package does not model -- hdiutil writes several -- survives untouched.
func RefreshChecksums(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("RefreshChecksums: %w", err)
	}
	defer f.Close()

	info, err := osStatFile(f)
	if err != nil {
		return fmt.Errorf("RefreshChecksums: %w", err)
	}
	if info.Size() < kolyBlockSize {
		return fmt.Errorf("RefreshChecksums: file too small for koly block")
	}
	kb := make([]byte, kolyBlockSize)
	if _, err := osReadAtFile(f, kb, info.Size()-kolyBlockSize); err != nil {
		return fmt.Errorf("RefreshChecksums: read koly: %w", err)
	}
	koly, err := parseKoly(kb)
	if err != nil {
		return fmt.Errorf("RefreshChecksums: %w", err)
	}
	if koly.segmentCount > 1 {
		return fmt.Errorf("RefreshChecksums: multi-segment images are not supported (segmentCount=%d)", koly.segmentCount)
	}

	plist := make([]byte, koly.xmlLength)
	if _, err := osReadAtFile(f, plist, int64(koly.xmlOffset)); err != nil {
		return fmt.Errorf("RefreshChecksums: read plist: %w", err)
	}
	items, err := parsePlistBlkx(plist)
	if err != nil {
		return fmt.Errorf("RefreshChecksums: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("RefreshChecksums: no blkx table in the plist")
	}

	// The sectors are reassembled only so the runs can be walked; the CRC that
	// matters comes back from fillSectorsFromRuns, which is the one place that
	// knows a zero-fill run contributes nothing to it.
	sectors := make([]byte, koly.sectorCount*udifSectorSize)
	tables := make([][]byte, len(items))
	for i, item := range items {
		tbl, runs, err := parseBlkxTable(item.Data)
		if err != nil {
			return fmt.Errorf("RefreshChecksums: parse blkx: %w", err)
		}
		sum, err := fillSectorsFromRuns(f, tbl, runs, sectors)
		if err != nil {
			return fmt.Errorf("RefreshChecksums: %w", err)
		}
		// Patched at the four bytes the value occupies rather than re-serialised,
		// so fields this package does not model -- BuffersNeeded, the descriptor,
		// anything hdiutil put there -- come through unchanged.
		patched := make([]byte, len(item.Data))
		copy(patched, item.Data)
		binary.BigEndian.PutUint32(patched[64:68], blkxChecksumCRC32)
		binary.BigEndian.PutUint32(patched[68:72], 32)
		binary.BigEndian.PutUint32(patched[72:76], sum)
		tables[i] = patched
	}

	newPlist, err := replaceBlkxData(plist, items, tables)
	if err != nil {
		return fmt.Errorf("RefreshChecksums: %w", err)
	}
	if _, err := osWriteAtFile(f, newPlist, int64(koly.xmlOffset)); err != nil {
		return fmt.Errorf("RefreshChecksums: write plist: %w", err)
	}

	h := crc32.NewIEEE()
	if _, err := io.Copy(h, io.NewSectionReader(f, int64(koly.dataForkOffset), int64(koly.dataForkLength))); err != nil {
		return fmt.Errorf("RefreshChecksums: reading the data fork: %w", err)
	}
	koly.dataForkChecksum = h.Sum32()
	koly.masterChecksum = masterChecksumOf(tables)
	out := kolyToBytes(koly)
	if _, err := osWriteAtFile(f, out[:], info.Size()-kolyBlockSize); err != nil {
		return fmt.Errorf("RefreshChecksums: write koly: %w", err)
	}
	return f.Sync()
}

// osWriteAtFile is the function used to write at an offset. Overridable in tests.
var osWriteAtFile = func(f *os.File, b []byte, off int64) (int, error) { return f.WriteAt(b, off) }

// replaceBlkxData substitutes each blkx table's base64 in the plist, leaving
// every other byte of it alone.
//
// The elements are found by CONTENT and not by position: a plist hdiutil wrote
// carries <data> elements that are not blkx tables at all -- 'plst' and 'nsiz'
// sit in the same resource-fork dict -- so the nth <data> is not the nth table.
// Each old table is matched against the base64 it decodes from, which is what
// parsePlistBlkx already read.
func replaceBlkxData(plist []byte, items []blkxPlistItem, tables [][]byte) ([]byte, error) {
	out := make([]byte, len(plist))
	copy(out, plist)
	for i, item := range items {
		want := base64.StdEncoding.EncodeToString(item.Data)
		got := base64.StdEncoding.EncodeToString(tables[i])
		lo, hi, ok := findDataSpan(out, want)
		if !ok {
			return nil, fmt.Errorf("blkx table %d is not in the plist as written", i)
		}
		// Same number of input bytes, so the same number of base64 characters;
		// the whitespace between them is kept where it was.
		copy(out[lo:hi], respace(string(out[lo:hi]), got))
	}
	return out, nil
}

// findDataSpan returns the bounds of the first <data> element whose content,
// with whitespace removed, is want.
func findDataSpan(plist []byte, want string) (int, int, bool) {
	s := string(plist)
	for from := 0; ; {
		i := strings.Index(s[from:], "<data>")
		if i < 0 {
			return 0, 0, false
		}
		lo := from + i + len("<data>")
		j := strings.Index(s[lo:], "</data>")
		if j < 0 {
			return 0, 0, false
		}
		hi := lo + j
		if stripSpace(s[lo:hi]) == want {
			return lo, hi, true
		}
		from = hi
	}
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// respace lays out fresh base64 over the whitespace skeleton of the old text.
// Both carry the same number of non-space characters, because they encode the
// same number of bytes.
func respace(old, fresh string) []byte {
	out := make([]byte, 0, len(old))
	k := 0
	for _, r := range old {
		if unicode.IsSpace(r) {
			out = append(out, string(r)...)
			continue
		}
		out = append(out, fresh[k])
		k++
	}
	return out
}
