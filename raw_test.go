package dmg

import (
	"bytes"
	"os"
	"path/filepath"
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
