package dmg

import (
	"bytes"
	"embed"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Four images written by hdiutil, one per compressed UDIF flavour, and the raw
// file all four were made from:
//
//	hdiutil convert raw.img -format {UDCO,UDZO,UDBZ,ULFO} -o out.dmg
//
// They are embedded rather than read off disk because the emulated CI lanes run
// a `go test -c` binary with no testdata/ beside it.
//
// This is the witness the package did not have. Every earlier test for a
// compressed flavour built its image with the package's own blkx constants, so
// a constant that named the wrong codec agreed with itself and passed. Against
// these four, three of the four flavours failed: UDCO was handed to the LZFSE
// decoder, and UDBZ and ULFO were refused as unknown block types.
//
//go:embed testdata/hdiutil-UDCO.dmg testdata/hdiutil-UDZO.dmg
//go:embed testdata/hdiutil-UDBZ.dmg testdata/hdiutil-ULFO.dmg
//go:embed testdata/hdiutil-raw.img
var appleImages embed.FS

// blockTypeFor names the blkx block type each flavour must be built out of, so
// a fixture that does not contain it fails as a fixture rather than passing as
// a decode.
var blockTypeFor = map[string]uint32{
	"UDCO": blkxADC,
	"UDZO": blkxZlib,
	"UDBZ": blkxBzip2,
	"ULFO": blkxLzfse,
}

func TestHdiutilImagesUnpackToTheSameBytes(t *testing.T) {
	want, err := appleImages.ReadFile("testdata/hdiutil-raw.img")
	if err != nil {
		t.Fatalf("read raw fixture: %v", err)
	}

	for _, format := range []string{"UDCO", "UDZO", "UDBZ", "ULFO"} {
		t.Run(format, func(t *testing.T) {
			path := writeFixture(t, "testdata/hdiutil-"+format+".dmg")

			// Premise: this image really is built out of the block type the
			// flavour uses. Without this the test would pass on a fixture that
			// had quietly been re-encoded as something else.
			assertContainsBlockType(t, path, format, blockTypeFor[format])

			got, err := DetectUDIFFormat(path)
			if err != nil {
				t.Fatalf("DetectUDIFFormat: %v", err)
			}
			if got != format {
				t.Errorf("DetectUDIFFormat = %q, want %q", got, format)
			}

			tmp, err := UnpackToTemp(path)
			if err != nil {
				t.Fatalf("UnpackToTemp: %v", err)
			}
			defer os.Remove(tmp)
			unpacked, err := os.ReadFile(tmp)
			if err != nil {
				t.Fatalf("read unpacked: %v", err)
			}
			if !bytes.Equal(unpacked, want) {
				t.Fatalf("unpacked %d bytes, want %d; first difference at %d",
					len(unpacked), len(want), firstDifference(unpacked, want))
			}
		})
	}
}

// A flavour this package can read but not write must be refused by name. That
// refusal is what stops a resize from re-encoding one: "UDRO" maps onto the raw
// writer, so a UDBZ misread as "UDRO" would have come back uncompressed.
func TestResizeRefusesFlavoursItCannotWrite(t *testing.T) {
	for _, format := range []string{"UDCO", "UDBZ", "ULFO"} {
		t.Run(format, func(t *testing.T) {
			path := writeFixture(t, "testdata/hdiutil-"+format+".dmg")
			err := Format{}.Resize(path, 8*1024)
			if err == nil {
				t.Fatalf("Resize succeeded on a %s image, which this package cannot write", format)
			}
			if !strings.Contains(err.Error(), format) {
				t.Errorf("Resize error = %v, want it to name %s", err, format)
			}
		})
	}
}

// UDZO is the control: the one flavour this package can write, so the refusal
// above has to be about the encoding and not about resizing in general.
func TestResizeAcceptsUDZO(t *testing.T) {
	path := writeFixture(t, "testdata/hdiutil-UDZO.dmg")
	if err := (Format{}).Resize(path, 8*1024); err != nil {
		t.Fatalf("Resize on UDZO: %v", err)
	}
}

func writeFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := appleImages.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(name))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func assertContainsBlockType(t *testing.T, path, format string, want uint32) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	trailer := make([]byte, kolyBlockSize)
	if _, err := f.ReadAt(trailer, info.Size()-kolyBlockSize); err != nil {
		t.Fatalf("read koly: %v", err)
	}
	koly, err := parseKoly(trailer)
	if err != nil {
		t.Fatalf("parseKoly: %v", err)
	}
	xml := make([]byte, koly.xmlLength)
	if _, err := f.ReadAt(xml, int64(koly.xmlOffset)); err != nil {
		t.Fatalf("read plist: %v", err)
	}
	items, err := parsePlistBlkx(xml)
	if err != nil {
		t.Fatalf("parsePlistBlkx: %v", err)
	}
	var seen []uint32
	for _, it := range items {
		_, runs, err := parseBlkxTable(it.Data)
		if err != nil {
			t.Fatalf("parseBlkxTable: %v", err)
		}
		for _, r := range runs {
			if r.blockType == want {
				return
			}
			seen = append(seen, r.blockType)
		}
	}
	t.Fatalf("the %s fixture contains no 0x%08x run (it has %#x): the decode below "+
		"would not reach the codec this case is about", format, want, seen)
}

func firstDifference(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
