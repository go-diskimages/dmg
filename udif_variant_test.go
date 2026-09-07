package dmg

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The koly checksum slots are written only when there is a value to put in
// them. The package itself never sets one (see writeUDIF), so these branches
// exist for a caller that computes Apple's variant one day.
func TestKolyChecksumSlotsRoundTrip(t *testing.T) {
	k := kolyBlock{
		sectorCount:      8,
		imageVariant:     imageVariantWholeDisk,
		dataForkChecksum: 0xDEADBEEF,
		masterChecksum:   0x0BADC0DE,
	}
	kb := kolyToBytes(k)
	got, err := parseKoly(kb[:])
	if err != nil {
		t.Fatalf("parseKoly: %v", err)
	}
	if got.dataForkChecksum != k.dataForkChecksum || got.masterChecksum != k.masterChecksum {
		t.Errorf("checksums = 0x%08x/0x%08x, want 0x%08x/0x%08x",
			got.dataForkChecksum, got.masterChecksum, k.dataForkChecksum, k.masterChecksum)
	}
	// …and with none set, the TYPE fields must stay 0, not claim CRC-32.
	bArr := kolyToBytes(kolyBlock{sectorCount: 8, imageVariant: imageVariantWholeDisk})
	b := bArr[:]
	if ty := binary.BigEndian.Uint32(b[80:84]); ty != 0 {
		t.Errorf("dataFork checksum type = %d with no value, want 0", ty)
	}
	if ty := binary.BigEndian.Uint32(b[352:356]); ty != 0 {
		t.Errorf("master checksum type = %d with no value, want 0", ty)
	}
}

// hdiutil emits blkxIgnore in every UDZO it writes; the package used to reject
// it as an unsupported block type, which made every Apple image unreadable.
func TestDecompressRunIgnoreIsZeroFill(t *testing.T) {
	out, err := decompressRun(strings.NewReader(""), 0,
		blkxRun{blockType: blkxIgnore, sectorCount: 2})
	if err != nil {
		t.Fatalf("decompressRun(ignore): %v", err)
	}
	if len(out) != 2*udifSectorSize {
		t.Fatalf("len = %d, want %d", len(out), 2*udifSectorSize)
	}
	for i, b := range out {
		if b != 0 {
			t.Fatalf("byte %d = %#x, want zero fill", i, b)
		}
	}
}

// The format is inferred from the chunk types, because it is not recorded
// anywhere in a UDIF file.
func TestDetectUDIFFormatInfersFromChunks(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		enc  runEncoding
		want string
	}{
		// Uncompressed and in a container is UDRO, not UDRW: no UDIF image
		// is UDRW. That name belongs to the raw image, which has no
		// container for this function to read.
		{"raw runs", encRaw, "UDRO"},
		{"zlib runs", encZlib, "UDZO"},
		{"elided zero runs", encSparse, "UDSP"},
	} {
		p := filepath.Join(dir, tc.name+".dmg")
		if err := writeUDIF(p, makeTestSectors(64), tc.enc); err != nil {
			t.Fatalf("writeUDIF %s: %v", tc.name, err)
		}
		got, err := DetectUDIFFormat(p)
		if err != nil {
			t.Fatalf("DetectUDIFFormat %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("DetectUDIFFormat(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A partitioned layout with no compressed chunks is a read-only raw image.
func TestDetectUDIFFormatPartitionedIsUDRO(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ro.dmg")
	if err := writeUDIF(p, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(raw[len(raw)-512+488:], imageVariantPartitioned)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DetectUDIFFormat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != "UDRO" {
		t.Errorf("partitioned raw = %q, want UDRO", got)
	}
}

func TestDetectUDIFFormatRejectsBrokenPlist(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.dmg")
	if err := writeUDIF(p, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	k := len(raw) - 512
	xmlOff := binary.BigEndian.Uint64(raw[k+216 : k+224])
	copy(raw[xmlOff:], []byte("<<<not a plist"))
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectUDIFFormat(p); err == nil {
		t.Error("expected an error on an unparsable plist")
	}

	// …and when the plist cannot even be READ, because the koly points past
	// the end of the file.
	q := filepath.Join(dir, "short.dmg")
	if err := writeUDIF(q, makeTestSectors(8), encRaw); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(q)
	if err != nil {
		t.Fatal(err)
	}
	k2 := len(raw2) - 512
	binary.BigEndian.PutUint64(raw2[k2+216:k2+224], uint64(len(raw2))+1<<20)
	if err := os.WriteFile(q, raw2, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectUDIFFormat(q); err == nil {
		t.Error("expected an error when the plist offset is past EOF")
	}
}

// hdiutil indents its <data> with TABS and wraps it across lines. Stripping
// only newlines and spaces made every Apple-produced image fail at input
// byte 0, while every image this package wrote itself decoded fine — which is
// exactly why no test caught it.
func TestPlistBase64ToleratesTabs(t *testing.T) {
	tbl := writeBlkxTable(blkxTable{sectorCount: 1}, []blkxRun{{blockType: blkxTerm}}, 0)
	b64 := base64.StdEncoding.EncodeToString(tbl)
	// re-wrap the way hdiutil does: newline + tabs every 60 characters
	var wrapped strings.Builder
	for i := 0; i < len(b64); i += 60 {
		j := min(i+60, len(b64))
		wrapped.WriteString("\n\t\t\t\t\t" + b64[i:j])
	}
	wrapped.WriteString("\n\t\t\t\t")
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>resource-fork</key><dict><key>blkx</key><array><dict>
<key>Attributes</key><string>0x0050</string>
<key>ID</key><string>0</string>
<key>Name</key><string>whole disk</string>
<key>Data</key><data>` + wrapped.String() + `</data>
</dict></array></dict></dict></plist>`
	items, err := parsePlistBlkx([]byte(xml))
	if err != nil {
		t.Fatalf("parsePlistBlkx with tab-indented data: %v", err)
	}
	if len(items) != 1 || !bytes.Equal(items[0].Data, tbl) {
		t.Fatalf("round trip failed: %d items", len(items))
	}
}

// decompressRun's terminator arm: a run list ends with blkxTerm, and asking
// for it directly must yield nothing rather than reading the data fork.
func TestDecompressRunTerminator(t *testing.T) {
	out, err := decompressRun(strings.NewReader(""), 0, blkxRun{blockType: blkxTerm})
	if err != nil {
		t.Fatalf("decompressRun(term): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("len = %d, want 0", len(out))
	}
}

// A blkx table that claims more sectors than the image holds must be clamped,
// not allowed to slice past the end of the buffer.
func TestBlkxChecksumClampsToImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "over.dmg")
	sectors := makeTestSectors(8)
	if err := writeUDIF(p, sectors, encRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Inflate the blkx table's sectorCount inside the base64 payload by
	// rewriting the plist with a doctored table.
	k := len(raw) - 512
	xmlOff := binary.BigEndian.Uint64(raw[k+216 : k+224])
	items, err := parsePlistBlkx(raw[xmlOff:k])
	if err != nil {
		t.Fatal(err)
	}
	tbl, runs, err := parseBlkxTable(items[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	tbl.sectorCount *= 4 // claim four times the image
	doctored := writeBlkxTable(tbl, runs, tbl.checksum)
	newPlist := writePlistBlkx([]blkxPlistItem{{Attributes: "0x0050", Data: doctored, ID: "0", Name: "whole disk"}})
	out := append(append([]byte(nil), raw[:xmlOff]...), newPlist...)
	koly, err := parseKoly(raw[k:])
	if err != nil {
		t.Fatal(err)
	}
	koly.xmlLength = uint64(len(newPlist))
	kb := kolyToBytes(koly)
	out = append(out, kb[:]...)
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	// The point is that it does not panic; a checksum complaint is fine.
	if _, _, err := readAllUDIFSectors(p); err != nil &&
		!strings.Contains(err.Error(), "checksum") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// LZFSE chunks mean ULFO, which this package can read but does not write.
func TestDetectUDIFFormatLZFSE(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ulfo.dmg")
	tbl := writeBlkxTable(blkxTable{sectorCount: 1},
		[]blkxRun{{blockType: blkxLzfse, sectorCount: 1}, {blockType: blkxTerm, sectorNumber: 1}}, 0)
	plist := writePlistBlkx([]blkxPlistItem{{Attributes: "0x0050", Data: tbl, ID: "0", Name: "whole disk"}})
	koly := kolyBlock{
		segmentNumber: 1, segmentCount: 1, sectorCount: 1,
		imageVariant: imageVariantWholeDisk,
		xmlOffset:    0, xmlLength: uint64(len(plist)),
	}
	kb := kolyToBytes(koly)
	if err := os.WriteFile(p, append(append([]byte(nil), plist...), kb[:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DetectUDIFFormat(p)
	if err != nil {
		t.Fatalf("DetectUDIFFormat: %v", err)
	}
	if got != "ULFO" {
		t.Errorf("got %q, want ULFO", got)
	}
}

// A blkx payload that is valid base64 but not a blkx table.
func TestDetectUDIFFormatRejectsBadBlkx(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "badblkx.dmg")
	plist := writePlistBlkx([]blkxPlistItem{{Attributes: "0x0050", Data: []byte("not a blkx table at all, but long enough to pass the size check........"), ID: "0", Name: "x"}})
	koly := kolyBlock{segmentNumber: 1, segmentCount: 1, sectorCount: 1,
		imageVariant: imageVariantWholeDisk, xmlLength: uint64(len(plist))}
	kb := kolyToBytes(koly)
	if err := os.WriteFile(p, append(append([]byte(nil), plist...), kb[:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectUDIFFormat(p); err == nil {
		t.Error("expected an error on a payload that is not a blkx table")
	}
}
