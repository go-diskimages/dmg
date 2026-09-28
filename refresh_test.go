package dmg

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patchByte flips one byte of the file in place, which is what a writer editing
// an image does and what the blkx checksum is there to notice.
func patchByte(t *testing.T, path string, off int64, v byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{v}, off); err != nil {
		t.Fatal(err)
	}
}

// TestAnInPlaceEditIsRefusedUntilTheChecksumsAreRefreshed is the whole reason
// this function exists, and it carries its own control: the read has to FAIL
// first, or the refresh afterwards proves nothing.
func TestAnInPlaceEditIsRefusedUntilTheChecksumsAreRefreshed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udrw.dmg")
	original := makeTestSectors(8)
	if err := writeUDIF(path, original, encRaw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAllUDIFSectors(path); err != nil {
		t.Fatalf("the image did not read back before it was touched: %v", err)
	}

	// encRaw stores the sectors at data-fork offset 0, so this is sector 0's
	// first byte.
	patchByte(t, path, 0, original[0]^0xff)

	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("an edited image read back clean: the checksums are not being checked, " +
			"and the rest of this test would prove nothing")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("refused for some other reason than a checksum: %v", err)
	}

	if err := RefreshChecksums(path); err != nil {
		t.Fatalf("RefreshChecksums: %v", err)
	}

	got, _, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatalf("still refused after a refresh: %v", err)
	}
	want := append([]byte(nil), original...)
	want[0] ^= 0xff
	if !bytes.Equal(got, want) {
		t.Error("the sectors read back are not the edited ones")
	}
}

// TestRefreshHonoursTheZeroFillRule is the case a consumer re-implementing this
// by hand gets wrong: a sparse image's blkx checksum covers the bytes the
// PRODUCING runs store and not the zero-filled span, so a CRC over every sector
// would produce a value this package's own reader then rejects.
func TestRefreshHonoursTheZeroFillRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udsp.dmg")
	// 4 sectors of data then 12 of zeros: buildRunsForSparse elides the zeros
	// into a run that stores nothing.
	original := makeTestSectors(16)
	if err := writeUDIF(path, original, encSparse); err != nil {
		t.Fatal(err)
	}
	sectors, koly, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sectors[4*udifSectorSize:], make([]byte, 12*udifSectorSize)) {
		t.Fatal("the fixture has no zero-filled span, so it cannot show the rule")
	}
	// A zero-fill run stores nothing, so the data fork is shorter than the image.
	if koly.dataForkLength >= uint64(len(original)) {
		t.Fatalf("data fork is %d bytes for %d sectors: nothing was elided",
			koly.dataForkLength, len(original)/udifSectorSize)
	}

	patchByte(t, path, 0, original[0]^0xff)
	if _, _, err := readAllUDIFSectors(path); err == nil {
		t.Fatal("an edited sparse image read back clean")
	}
	if err := RefreshChecksums(path); err != nil {
		t.Fatalf("RefreshChecksums: %v", err)
	}
	got, _, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatalf("a refreshed sparse image is refused -- the zero-fill rule was not "+
			"applied the same way on both sides: %v", err)
	}
	if got[0] != original[0]^0xff {
		t.Error("the edited byte did not survive")
	}
}

// rewritePlist replaces the plist of an image and moves the koly to follow it,
// so a test can hand RefreshChecksums a plist it did not write itself.
func rewritePlist(t *testing.T, path string, edit func(string) string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	koly, err := parseKoly(raw[len(raw)-kolyBlockSize:])
	if err != nil {
		t.Fatal(err)
	}
	old := string(raw[koly.xmlOffset : koly.xmlOffset+koly.xmlLength])
	fresh := edit(old)
	koly.xmlLength = uint64(len(fresh))
	kb := kolyToBytes(koly)
	out := append([]byte(nil), raw[:koly.xmlOffset]...)
	out = append(out, fresh...)
	out = append(out, kb[:]...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshKeepsPlistKeysThisPackageDoesNotModel: the plist is edited, not
// regenerated. writePlistBlkx emits four keys; hdiutil writes more, and
// rebuilding the file from the parsed struct would silently drop the rest.
func TestRefreshKeepsPlistKeysThisPackageDoesNotModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extra.dmg")
	if err := writeUDIF(path, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	rewritePlist(t, path, func(s string) string {
		return strings.Replace(s,
			"<key>Attributes</key>",
			"<key>CFName</key><string>whole disk</string>\n\t\t\t\t<key>Attributes</key>", 1)
	})
	patchByte(t, path, 0, 0xaa)
	if err := RefreshChecksums(path); err != nil {
		t.Fatalf("RefreshChecksums: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("<key>CFName</key>")) {
		t.Error("the key this package does not model was dropped")
	}
	if _, _, err := readAllUDIFSectors(path); err != nil {
		t.Fatalf("the refreshed image does not read back: %v", err)
	}
}

// TestRefreshFindsTheTableAmongOtherDataElements: a plist hdiutil wrote carries
// <data> elements that are not blkx tables, so the nth one is not the nth
// table. The match is by content.
func TestRefreshFindsTheTableAmongOtherDataElements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plst.dmg")
	if err := writeUDIF(path, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	rewritePlist(t, path, func(s string) string {
		return strings.Replace(s,
			"<key>blkx</key>",
			"<key>plst</key>\n\t\t<array>\n\t\t\t<dict>\n\t\t\t\t<key>Data</key><data>AAAA</data>\n\t\t\t</dict>\n\t\t</array>\n\t\t<key>blkx</key>", 1)
	})
	patchByte(t, path, 0, 0xbb)
	if err := RefreshChecksums(path); err != nil {
		t.Fatalf("RefreshChecksums: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("<data>AAAA</data>")) {
		t.Error("the unrelated <data> element was overwritten")
	}
	if _, _, err := readAllUDIFSectors(path); err != nil {
		t.Fatalf("the refreshed image does not read back: %v", err)
	}
}

// TestRespaceKeepsTheLayoutOfAWrappedBlob: hdiutil indents its <data> with tabs
// and newlines. Writing the fresh base64 over that skeleton is what keeps the
// plist the same length, so the koly's xmlLength and every offset after it stay
// true.
func TestRespaceKeepsTheLayoutOfAWrappedBlob(t *testing.T) {
	old := "\n\t\tAAAA\n\t\tBBBB\n\t"
	got := string(respace(old, "WXYZwxyz"))
	if want := "\n\t\tWXYZ\n\t\twxyz\n\t"; got != want {
		t.Errorf("respace = %q, want %q", got, want)
	}
	if len(got) != len(old) {
		t.Errorf("length changed: %d -> %d", len(old), len(got))
	}
}

func TestRefreshChecksumsRejectsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	tiny := filepath.Join(dir, "tiny.dmg")
	if err := os.WriteFile(tiny, make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	notKoly := filepath.Join(dir, "notkoly.dmg")
	if err := os.WriteFile(notKoly, make([]byte, kolyBlockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path, want string }{
		{"no such file", filepath.Join(dir, "absent.dmg"), "RefreshChecksums:"},
		{"too small for a koly", tiny, "too small"},
		{"not a koly block", notKoly, "RefreshChecksums:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := RefreshChecksums(c.path)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestRefreshChecksumsRefusesAMultiSegmentImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	koly, err := parseKoly(raw[len(raw)-kolyBlockSize:])
	if err != nil {
		t.Fatal(err)
	}
	koly.segmentCount = 2
	kb := kolyToBytes(koly)
	copy(raw[len(raw)-kolyBlockSize:], kb[:])
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = RefreshChecksums(path)
	if err == nil || !strings.Contains(err.Error(), "multi-segment") {
		t.Errorf("err = %v, want a refusal naming multi-segment", err)
	}
}

func TestRefreshChecksumsRefusesAPlistItCannotUse(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			name: "not a plist at all",
			edit: func(string) string { return "<plist" },
			want: "RefreshChecksums:",
		},
		{
			name: "an empty blkx array",
			edit: func(s string) string {
				i := strings.Index(s, "<key>blkx</key>")
				return s[:i] + "<key>blkx</key>\n\t\t<array>\n\t\t</array>\n\t</dict>\n</dict>\n</plist>\n"
			},
			want: "no blkx table",
		},
		{
			name: "a blkx blob that is not a table",
			edit: func(s string) string {
				lo := strings.Index(s, "<data>") + len("<data>")
				hi := strings.Index(s, "</data>")
				return s[:lo] + "AAAA" + s[hi:]
			},
			want: "parse blkx",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.dmg")
			if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
				t.Fatal(err)
			}
			rewritePlist(t, path, c.edit)
			err := RefreshChecksums(path)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestRefreshChecksumsReportsARunItCannotRead: a table whose run points past the
// end of the file cannot be walked, and the failure has to say so rather than
// write checksums over whatever happened to be in the buffer.
func TestRefreshChecksumsReportsARunItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	rewritePlist(t, path, func(s string) string {
		lo := strings.Index(s, "<data>") + len("<data>")
		hi := strings.Index(s, "</data>")
		tbl := decodeBase64ForTest(t, s[lo:hi])
		// The first run's compressedOffset, moved far past the data fork.
		binary.BigEndian.PutUint64(tbl[blkxHeaderSize+24:blkxHeaderSize+32], 1<<40)
		return s[:lo] + encodeBase64ForTest(tbl) + s[hi:]
	})
	err := RefreshChecksums(path)
	if err == nil || !strings.Contains(err.Error(), "RefreshChecksums:") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// TestReplaceBlkxDataSaysWhenTheTableIsNotThere guards the one branch that
// cannot be reached through a real image: parsePlistBlkx and replaceBlkxData
// read the same plist, so they agree unless one of them changes.
func TestReplaceBlkxDataSaysWhenTheTableIsNotThere(t *testing.T) {
	_, err := replaceBlkxData([]byte("<plist></plist>"),
		[]blkxPlistItem{{Data: []byte{1, 2, 3}}}, [][]byte{{4, 5, 6}})
	if err == nil || !strings.Contains(err.Error(), "not in the plist") {
		t.Errorf("err = %v, want it to say the table is not in the plist", err)
	}
	// An unterminated element is not a match either.
	_, err = replaceBlkxData([]byte("<data>AQID"),
		[]blkxPlistItem{{Data: []byte{1, 2, 3}}}, [][]byte{{4, 5, 6}})
	if err == nil {
		t.Error("an unterminated <data> was accepted")
	}
}

func TestRefreshChecksumsReportsAFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, want string }{
		{"plist", "write plist"},
		{"koly", "write koly"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := osWriteAtFile
			calls := 0
			osWriteAtFile = func(f *os.File, b []byte, off int64) (int, error) {
				calls++
				if (c.name == "plist" && calls == 1) || (c.name == "koly" && calls == 2) {
					return 0, os.ErrInvalid
				}
				return f.WriteAt(b, off)
			}
			defer func() { osWriteAtFile = old }()
			err := RefreshChecksums(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestRefreshChecksumsReportsAFailedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rr.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		nth  int
		want string
	}{
		{"koly", 1, "read koly"},
		{"plist", 2, "read plist"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := osReadAtFile
			calls := 0
			osReadAtFile = func(f *os.File, b []byte, off int64) (int, error) {
				calls++
				if calls == c.nth {
					return 0, os.ErrInvalid
				}
				return f.ReadAt(b, off)
			}
			defer func() { osReadAtFile = old }()
			err := RefreshChecksums(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestRefreshChecksumsReportsAFailedStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "st.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	old := osStatFile
	osStatFile = func(*os.File) (os.FileInfo, error) { return nil, os.ErrInvalid }
	defer func() { osStatFile = old }()
	if err := RefreshChecksums(path); err == nil {
		t.Error("a failed stat was accepted")
	}
}

func decodeBase64ForTest(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(stripSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func encodeBase64ForTest(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// TestRefreshRewritesAWrappedDataBlob: hdiutil wraps its base64 over many lines
// with tabs. The fresh value is laid over that skeleton, so the plist keeps its
// length and the koly's xmlLength stays true.
func TestRefreshRewritesAWrappedDataBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrapped.dmg")
	if err := writeUDIF(path, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritePlist(t, path, func(s string) string {
		lo := strings.Index(s, "<data>") + len("<data>")
		hi := strings.Index(s, "</data>")
		var b strings.Builder
		for i, c := range s[lo:hi] {
			if i%64 == 0 {
				b.WriteString("\n\t\t\t\t")
			}
			b.WriteRune(c)
		}
		b.WriteString("\n\t\t\t\t")
		return s[:lo] + b.String() + s[hi:]
	})
	grew, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if grew.Size() <= before.Size() {
		t.Fatal("the fixture is not wrapped, so it cannot show the rule")
	}
	patchByte(t, path, 0, 0xcc)
	if err := RefreshChecksums(path); err != nil {
		t.Fatalf("RefreshChecksums: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != grew.Size() {
		t.Errorf("the plist changed length: %d -> %d", grew.Size(), after.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("\n\t\t\t\t")) {
		t.Error("the wrapping was flattened")
	}
	if _, _, err := readAllUDIFSectors(path); err != nil {
		t.Fatalf("the refreshed image does not read back: %v", err)
	}
}

// TestRefreshRefusesATableItCannotFindAgain reaches the one branch a real image
// cannot: the plist parser resolves XML entities, so the text in the file is not
// always the base64 the parser handed back, and the substitution has to say so
// rather than leave a table unpatched while the koly claims it was.
func TestRefreshRefusesATableItCannotFindAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entity.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	rewritePlist(t, path, func(s string) string {
		lo := strings.Index(s, "<data>") + len("<data>")
		return s[:lo] + fmt.Sprintf("&#%d;", s[lo]) + s[lo+1:]
	})
	err := RefreshChecksums(path)
	if err == nil || !strings.Contains(err.Error(), "not in the plist") {
		t.Errorf("err = %v, want it to say the table is not in the plist as written", err)
	}
}

// TestRefreshReportsADataForkItCannotRead: an offset that does not fit in an
// int64 becomes negative, and a section reader refuses it rather than reading
// nothing and calling that a checksum.
func TestRefreshReportsADataForkItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork.dmg")
	if err := writeUDIF(path, makeTestSectors(4), encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	koly, err := parseKoly(raw[len(raw)-kolyBlockSize:])
	if err != nil {
		t.Fatal(err)
	}
	koly.dataForkOffset = 1 << 63
	kb := kolyToBytes(koly)
	copy(raw[len(raw)-kolyBlockSize:], kb[:])
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = RefreshChecksums(path)
	if err == nil || !strings.Contains(err.Error(), "reading the data fork") {
		t.Errorf("err = %v, want it to name the data fork", err)
	}
}
