package dmg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A raw image is a first-class input: the sectors ARE the file, which is what
// hdiutil writes for -format UDRW and the only shape macOS mounts read/write.
func TestRawImageIsReadAndWritten(t *testing.T) {
	dir := t.TempDir()
	original := makeTestSectors(64)
	raw := filepath.Join(dir, "image.raw")
	if err := os.WriteFile(raw, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if IsUDIF(raw) {
		t.Error("a raw image was taken for a UDIF one")
	}
	got, err := readSectors(raw)
	if err != nil {
		t.Fatalf("readSectors: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Error("the sectors came back changed")
	}

	// …and it converts to a container and back without loss.
	zo := filepath.Join(dir, "image.dmg")
	if err := ConvertUDIF(raw, zo, "UDZO"); err != nil {
		t.Fatalf("raw → UDZO: %v", err)
	}
	if f, _ := DetectUDIFFormat(zo); f != "UDZO" {
		t.Errorf("format = %q, want UDZO", f)
	}
	back := filepath.Join(dir, "back.raw")
	if err := ConvertUDIF(zo, back, "UDRW"); err != nil {
		t.Fatalf("UDZO → raw: %v", err)
	}
	b, err := os.ReadFile(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, original) {
		t.Error("the round trip through a container changed the bytes")
	}
}

// A file that is not an image is refused rather than padded into one.
func TestAFileThatIsNotAnImageIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"a partial sector", make([]byte, 700)},
	} {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, tc.body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSectors(p); err == nil {
			t.Errorf("%s: readSectors accepted it", tc.name)
		}
		if ok, err := InPlaceWritable(p); err != nil || ok {
			t.Errorf("%s: InPlaceWritable = %v, %v; want false", tc.name, ok, err)
		}
	}
	if _, err := readSectors(filepath.Join(dir, "nowhere")); err == nil {
		t.Error("readSectors accepted a file that is not there")
	}
	if _, err := InPlaceWritable(filepath.Join(dir, "nowhere")); err == nil {
		t.Error("InPlaceWritable accepted a file that is not there")
	}
}

// The question a caller usually means when it asks about "UDRW": can I map
// ReadAt and WriteAt straight onto this file?
func TestInPlaceWritable(t *testing.T) {
	dir := t.TempDir()
	sectors := makeTestSectors(64)
	for _, tc := range []struct {
		name string
		enc  runEncoding
		want bool
	}{
		{"raw runs", encRaw, true},
		{"zlib runs", encZlib, false},
		{"elided zero runs", encSparse, false},
	} {
		p := filepath.Join(dir, tc.name+".dmg")
		if err := writeUDIF(p, sectors, tc.enc); err != nil {
			t.Fatal(err)
		}
		got, err := InPlaceWritable(p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: InPlaceWritable = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The raw image, where the sectors are the whole file.
	p := filepath.Join(dir, "image.raw")
	if err := os.WriteFile(p, sectors, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := InPlaceWritable(p); err != nil || !got {
		t.Errorf("a raw image: InPlaceWritable = %v, %v; want true", got, err)
	}
}

// ⛔ The defect the rename exposed: both resize paths passed the koly's
// imageVariant to a switch comparing it against FORMAT codes. It never
// matched, so growing a compressed image quietly rewrote it uncompressed —
// the caller asked for more room and got a different image.
func TestGrowingAnImageKeepsItsEncoding(t *testing.T) {
	dir := t.TempDir()
	sectors := makeTestSectors(2048)
	for _, tc := range []struct {
		name string
		enc  runEncoding
		want string
	}{
		{"zlib runs", encZlib, "UDZO"},
		{"elided zero runs", encSparse, "UDSP"},
		{"raw runs", encRaw, "UDRO"},
	} {
		p := filepath.Join(dir, tc.name+".dmg")
		if err := writeUDIF(p, sectors, tc.enc); err != nil {
			t.Fatal(err)
		}
		if err := ResizeUDRW(p, 4096*udifSectorSize); err != nil {
			t.Fatalf("%s: ResizeUDRW: %v", tc.name, err)
		}
		if got, err := DetectUDIFFormat(p); err != nil || got != tc.want {
			t.Errorf("%s: after growing, format = %q (%v), want %q", tc.name, got, err, tc.want)
		}
		got, err := readSectors(p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 4096*udifSectorSize {
			t.Errorf("%s: %d bytes after growing, want %d", tc.name, len(got), 4096*udifSectorSize)
		}
		if !bytes.Equal(got[:len(sectors)], sectors) {
			t.Errorf("%s: growing changed the sectors that were already there", tc.name)
		}
	}
}

// A raw image grows too, and stays raw.
func TestGrowingARawImage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "image.raw")
	sectors := makeTestSectors(8)
	if err := os.WriteFile(p, sectors, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ResizeUDRW(p, 16*udifSectorSize); err != nil {
		t.Fatalf("ResizeUDRW: %v", err)
	}
	if IsUDIF(p) {
		t.Error("growing a raw image wrapped it in a container")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 16*udifSectorSize || !bytes.Equal(b[:len(sectors)], sectors) {
		t.Errorf("%d bytes, and the original sectors %s", len(b), map[bool]string{true: "survived", false: "did NOT survive"}[bytes.Equal(b[:len(sectors)], sectors)])
	}
}

// buildImage writes a UDIF image with the given runs, so a test can hand the
// reader shapes the writer never produces.
func buildImage(t *testing.T, path string, runs []blkxRun) {
	t.Helper()
	tbl := writeBlkxTable(blkxTable{sectorCount: 1}, runs, 0)
	plist := writePlistBlkx([]blkxPlistItem{{Attributes: "0x0050", Data: tbl, ID: "0", Name: "whole disk"}})
	koly := kolyToBytes(kolyBlock{
		segmentNumber: 1, segmentCount: 1, sectorCount: 1,
		imageVariant: imageVariantWholeDisk,
		xmlOffset:    0, xmlLength: uint64(len(plist)),
	})
	if err := os.WriteFile(path, append(append([]byte(nil), plist...), koly[:]...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every way the file underneath can refuse to answer. The seams are what make
// these reachable: a test that depended on not being root would pass by
// skipping, which is not the same as passing.
func TestTheFileUnderneathRefusing(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "image.raw")
	if err := os.WriteFile(good, makeTestSectors(4), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")

	t.Run("stat", func(t *testing.T) {
		defer restoreSeams(saveSeams())
		osStatFile = func(*os.File) (os.FileInfo, error) { return nil, boom }
		if _, err := readSectors(good); !errors.Is(err, boom) {
			t.Errorf("readSectors = %v", err)
		}
		if _, err := InPlaceWritable(good); !errors.Is(err, boom) {
			t.Errorf("InPlaceWritable = %v", err)
		}
	})
	t.Run("stat, on the second question", func(t *testing.T) {
		defer restoreSeams(saveSeams())
		real := osStatFile
		n := 0
		osStatFile = func(f *os.File) (os.FileInfo, error) {
			if n++; n > 1 {
				return nil, boom
			}
			return real(f)
		}
		if _, err := InPlaceWritable(good); !errors.Is(err, boom) {
			t.Errorf("InPlaceWritable = %v", err)
		}
	})
	t.Run("reading the trailer", func(t *testing.T) {
		defer restoreSeams(saveSeams())
		osReadAtFile = func(*os.File, []byte, int64) (int, error) { return 0, boom }
		if _, err := readSectors(good); !errors.Is(err, boom) {
			t.Errorf("readSectors = %v", err)
		}
	})
	t.Run("reading the whole file", func(t *testing.T) {
		defer restoreSeams(saveSeams())
		osReadWholeFile = func(string) ([]byte, error) { return nil, boom }
		if _, err := readSectors(good); !errors.Is(err, boom) {
			t.Errorf("readSectors = %v", err)
		}
	})
	t.Run("a file that is not there", func(t *testing.T) {
		gone := filepath.Join(dir, "gone")
		if _, err := readSectors(gone); err == nil {
			t.Error("readSectors accepted it")
		}
		if _, err := InPlaceWritable(gone); err == nil {
			t.Error("InPlaceWritable accepted it")
		}
		if err := writeSectors(gone, nil); err == nil {
			t.Error("writeSectors accepted it")
		}
	})
}

type seams struct {
	stat   func(*os.File) (os.FileInfo, error)
	readAt func(*os.File, []byte, int64) (int, error)
	whole  func(string) ([]byte, error)
}

func saveSeams() seams { return seams{osStatFile, osReadAtFile, osReadWholeFile} }
func restoreSeams(s seams) {
	osStatFile, osReadAtFile, osReadWholeFile = s.stat, s.readAt, s.whole
}

// A container whose own description cannot be read is refused rather than
// guessed at.
func TestAnUnreadableDescription(t *testing.T) {
	dir := t.TempDir()

	// The koly points its plist past the end of the file.
	past := filepath.Join(dir, "past.dmg")
	if err := writeUDIF(past, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(past)
	if err != nil {
		t.Fatal(err)
	}
	k := len(raw) - kolyBlockSize
	binary.BigEndian.PutUint64(raw[k+216:k+224], uint64(len(raw))+1<<20)
	if err := os.WriteFile(past, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InPlaceWritable(past); err == nil {
		t.Error("InPlaceWritable read a plist that is not there")
	}

	// The plist is where it says, and is not a plist.
	broken := filepath.Join(dir, "broken.dmg")
	if err := writeUDIF(broken, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	k2 := len(raw2) - kolyBlockSize
	xmlOff := binary.BigEndian.Uint64(raw2[k2+216 : k2+224])
	copy(raw2[xmlOff:], []byte("<<<not a plist"))
	if err := os.WriteFile(broken, raw2, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InPlaceWritable(broken); err == nil {
		t.Error("InPlaceWritable accepted an unparsable plist")
	}
	if err := writeSectors(broken, makeTestSectors(8)); err == nil {
		t.Error("writeSectors rewrote an image it could not read")
	}

	// A blkx payload that is valid base64 and not a blkx table.
	badRuns := filepath.Join(dir, "badruns.dmg")
	plist := writePlistBlkx([]blkxPlistItem{{Attributes: "0x0050", Data: []byte("nope"), ID: "0", Name: "whole disk"}})
	koly := kolyToBytes(kolyBlock{segmentNumber: 1, segmentCount: 1, sectorCount: 1,
		imageVariant: imageVariantWholeDisk, xmlOffset: 0, xmlLength: uint64(len(plist))})
	if err := os.WriteFile(badRuns, append(append([]byte(nil), plist...), koly[:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InPlaceWritable(badRuns); err == nil {
		t.Error("InPlaceWritable accepted a blkx table that is not one")
	}
}

// An encoding this package can read and not write is refused, rather than
// rewritten as something else — which is exactly the defect this change was
// about.
func TestRewritingAnEncodingWeCannotWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ulfo.dmg")
	buildImage(t, p, []blkxRun{{blockType: blkxLzfse, sectorCount: 1}, {blockType: blkxTerm, sectorNumber: 1}})
	if got, _ := DetectUDIFFormat(p); got != "ULFO" {
		t.Fatalf("format = %q, want ULFO", got)
	}
	err := writeSectors(p, makeTestSectors(1))
	if err == nil || !strings.Contains(err.Error(), "ULFO") {
		t.Errorf("writeSectors = %v, want a refusal naming ULFO", err)
	}
	// …and InPlaceWritable says no, because an lzfse run is not one-to-one.
	if ok, err := InPlaceWritable(p); err != nil || ok {
		t.Errorf("InPlaceWritable = %v, %v; want false", ok, err)
	}
}

// A destination that cannot be written is reported, not swallowed.
func TestConvertToAnImpossibleDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dmg")
	if err := writeUDIF(src, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	if err := ConvertUDIF(src, dir, "UDZO"); err == nil {
		t.Error("ConvertUDIF wrote a container over a directory")
	}
	if err := ConvertUDIF(src, dir, "UDRW"); err == nil {
		t.Error("ConvertUDIF wrote a raw image over a directory")
	}
}

// A UDIF image whose plist is unreadable is still a UDIF image. Answering
// otherwise would hand it to whatever handles raw files.
func TestABrokenContainerIsStillAContainer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broken.dmg")
	if err := writeUDIF(p, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	k := len(raw) - kolyBlockSize
	xmlOff := binary.BigEndian.Uint64(raw[k+216 : k+224])
	copy(raw[xmlOff:], []byte("<<<not a plist"))
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsUDIF(p) {
		t.Error("IsUDIF said no to an image that has a koly trailer")
	}
	if _, err := DetectUDIFFormat(p); err == nil {
		t.Error("DetectUDIFFormat read a plist that is not one")
	}
}
