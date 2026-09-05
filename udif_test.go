package dmg

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-compressions/lzfse"
)

// makeTestSectors returns n sectors of udifSectorSize bytes.
// The first 4 sectors have a distinct non-zero pattern; the rest are zero.
func makeTestSectors(n int) []byte {
	data := make([]byte, n*udifSectorSize)
	for i := 0; i < 4 && i < n; i++ {
		for j := range data[i*udifSectorSize : (i+1)*udifSectorSize] {
			data[i*udifSectorSize+j] = byte(i*16 + j%16 + 1)
		}
	}
	return data
}

func TestDetectUDIFFormat_UDRW(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.dmg")
	const nSectors = 2048 // 1 MiB
	if err := writeUDIF(path, makeTestSectors(nSectors), udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}
	got, err := DetectUDIFFormat(path)
	if err != nil {
		t.Fatalf("DetectUDIFFormat: %v", err)
	}
	if got != "UDRW" {
		t.Fatalf("expected UDRW, got %q", got)
	}
}

func TestDetectUDIFFormat_NotUDIF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := DetectUDIFFormat(path)
	if err == nil {
		t.Fatal("expected error for non-UDIF file")
	}
}

func TestConvertUDIF_UDRW_to_UDSP_and_back(t *testing.T) {
	dir := t.TempDir()
	original := makeTestSectors(2048) // 1 MiB
	srcPath := filepath.Join(dir, "src.udif")
	if err := writeUDIF(srcPath, original, udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}

	// UDRW → UDSP
	sparsePath := filepath.Join(dir, "sparse.udif")
	if err := ConvertUDIF(srcPath, sparsePath, "UDSP"); err != nil {
		t.Fatalf("ConvertUDIF UDRW→UDSP: %v", err)
	}
	if fmt, _ := DetectUDIFFormat(sparsePath); fmt != "UDSP" {
		t.Fatalf("expected UDSP after conversion, got %q", fmt)
	}

	// Sparse file should be smaller than the original (mostly zeros).
	sparseInfo, _ := os.Stat(sparsePath)
	srcInfo, _ := os.Stat(srcPath)
	if sparseInfo.Size() >= srcInfo.Size() {
		t.Logf("note: sparse file (%d) not smaller than raw (%d) – zero-heavy image expected smaller",
			sparseInfo.Size(), srcInfo.Size())
	}

	// UDSP → UDRW round-trip
	dstPath := filepath.Join(dir, "dst.udif")
	if err := ConvertUDIF(sparsePath, dstPath, "UDRW"); err != nil {
		t.Fatalf("ConvertUDIF UDSP→UDRW: %v", err)
	}
	if fmt, _ := DetectUDIFFormat(dstPath); fmt != "UDRW" {
		t.Fatalf("expected UDRW after round-trip, got %q", fmt)
	}

	// Sector data must survive the round-trip.
	recovered, _, err := readAllUDIFSectors(dstPath)
	if err != nil {
		t.Fatalf("readAllUDIFSectors: %v", err)
	}
	if !bytes.Equal(original, recovered) {
		t.Fatal("sector data mismatch after UDRW→UDSP→UDRW round-trip")
	}
}

func TestResizeUDRW_Grow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.udif")
	original := makeTestSectors(2048) // 1 MiB
	if err := writeUDIF(path, original, udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}

	const newSize = 2 * 1024 * 1024 // 2 MiB
	if err := ResizeUDRW(path, newSize); err != nil {
		t.Fatalf("ResizeUDRW: %v", err)
	}

	sectors, koly, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatalf("readAllUDIFSectors after resize: %v", err)
	}
	want := int64(newSize / udifSectorSize)
	if int64(koly.sectorCount) != want {
		t.Fatalf("sectorCount = %d, want %d", koly.sectorCount, want)
	}
	// Original data must be intact.
	if !bytes.Equal(original, sectors[:len(original)]) {
		t.Fatal("original sector data corrupted after resize")
	}
	// New bytes must be zero.
	for i, b := range sectors[len(original):] {
		if b != 0 {
			t.Fatalf("grown region byte %d = %d, want 0", i, b)
		}
	}
}

func TestResizeUDRW_Shrink_Error(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.udif")
	if err := writeUDIF(path, makeTestSectors(2048), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	if err := ResizeUDRW(path, 512*1024); err == nil {
		t.Fatal("expected error when shrinking")
	}
}

func TestResizeUDRW_Noop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.udif")
	if err := writeUDIF(path, makeTestSectors(2048), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	if err := ResizeUDRW(path, 1024*1024); err != nil {
		t.Fatalf("ResizeUDRW noop: %v", err)
	}
}

func TestConvertUDIF_UnknownFormat(t *testing.T) {
	if err := ConvertUDIF("src", "dst", "UNKNOWN"); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestDetectUDIFFormat_FileTooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny")
	if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := DetectUDIFFormat(path)
	if err == nil {
		t.Fatal("expected error for file smaller than koly block")
	}
}

func TestDetectUDIFFormat_FileNotExist(t *testing.T) {
	_, err := DetectUDIFFormat(filepath.Join(t.TempDir(), "nofile"))
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestDetectUDIFFormat_UnknownVariant(t *testing.T) {
	// Build a koly block with an unknown image variant (0xDEAD).
	path := filepath.Join(t.TempDir(), "unknown.udif")
	koly := kolyBlock{imageVariant: 0xDEAD, sectorCount: 1, segmentNumber: 1, segmentCount: 1}
	kb := kolyToBytes(koly)
	// Write enough data to pass the size check, then append koly.
	padding := make([]byte, 4096)
	data := append(padding, kb[:]...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := DetectUDIFFormat(path)
	if err == nil {
		t.Fatal("expected error for unknown image variant")
	}
}

func zlibCompress(in []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, 1)
	w.Write(in)
	w.Close()
	return buf.Bytes()
}

func TestDecompressRun_Zlib(t *testing.T) {
	const nSectors = 2
	sectors := makeTestSectors(nSectors)
	compressed := zlibCompress(sectors)

	run := blkxRun{
		blockType: blkxZlib, sectorNumber: 0, sectorCount: nSectors,
		compressedOffset: 0, compressedLength: uint64(len(compressed)),
	}
	got, err := decompressRun(bytes.NewReader(compressed), 0, run)
	if err != nil {
		t.Fatalf("decompressRun zlib: %v", err)
	}
	if !bytes.Equal(got, sectors) {
		t.Fatal("zlib decompression produced wrong data")
	}
}

func TestDecompressRun_Free(t *testing.T) {
	run := blkxRun{blockType: blkxFree, sectorNumber: 0, sectorCount: 2}
	got, err := decompressRun(bytes.NewReader(nil), 0, run)
	if err != nil {
		t.Fatalf("decompressRun free: %v", err)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("byte %d = %d, want 0", i, b)
		}
	}
}

func TestDecompressRun_Unsupported(t *testing.T) {
	run := blkxRun{blockType: 0x80000008, sectorNumber: 0, sectorCount: 1}
	_, err := decompressRun(bytes.NewReader(nil), 0, run)
	if err == nil {
		t.Fatal("expected error for unsupported block type")
	}
}

func TestDecompressRun_Lzfse(t *testing.T) {
	const nSectors = 2
	sectors := makeTestSectors(nSectors)
	compressed, err := lzfse.Compress(sectors)
	if err != nil {
		t.Fatalf("lzfse.Compress: %v", err)
	}
	run := blkxRun{
		blockType: blkxLzfse, sectorNumber: 0, sectorCount: nSectors,
		compressedOffset: 0, compressedLength: uint64(len(compressed)),
	}
	got, err := decompressRun(bytes.NewReader(compressed), 0, run)
	if err != nil {
		t.Fatalf("decompressRun lzfse: %v", err)
	}
	if !bytes.Equal(got, sectors) {
		t.Fatal("lzfse decompression produced wrong data")
	}
}

func TestConvertUDIF_UDRW_to_UDZO_and_back(t *testing.T) {
	dir := t.TempDir()
	original := makeTestSectors(4096) // 2 MiB, spans multiple zlib chunks
	srcPath := filepath.Join(dir, "src.udif")
	if err := writeUDIF(srcPath, original, udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}

	// UDRW → UDZO
	zoPath := filepath.Join(dir, "zlib.udif")
	if err := ConvertUDIF(srcPath, zoPath, "UDZO"); err != nil {
		t.Fatalf("ConvertUDIF UDRW→UDZO: %v", err)
	}
	if fmt, _ := DetectUDIFFormat(zoPath); fmt != "UDZO" {
		t.Fatalf("expected UDZO after conversion, got %q", fmt)
	}

	// UDZO → UDRW round-trip
	dstPath := filepath.Join(dir, "dst.udif")
	if err := ConvertUDIF(zoPath, dstPath, "UDRW"); err != nil {
		t.Fatalf("ConvertUDIF UDZO→UDRW: %v", err)
	}
	recovered, _, err := readAllUDIFSectors(dstPath)
	if err != nil {
		t.Fatalf("readAllUDIFSectors after UDZO round-trip: %v", err)
	}
	if !bytes.Equal(recovered, original) {
		t.Fatal("UDZO round-trip: sector data mismatch")
	}
}

func TestConvertUDIF_UnsupportedWrite(t *testing.T) {
	for _, fmt := range []string{"UDCO", "UDBZ"} {
		err := ConvertUDIF("src.dmg", "dst.dmg", fmt)
		if err == nil {
			t.Errorf("ConvertUDIF %q: expected error, got nil", fmt)
		}
	}
}

func TestWriteUDIF_Checksums(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ck.dmg")
	sectors := makeTestSectors(2048)
	if err := writeUDIF(path, sectors, udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}
	// Read back the koly block. Its two checksum slots are deliberately
	// EMPTY, and the blkx table carries the checksum instead.
	//
	// This test used to assert the opposite. Declaring type 2 in the koly
	// made hdiutil validate a value it rejected -- measured on macOS 26,
	// "invalid checksum", with everything else held constant -- so the image
	// would not attach at all. An unverified image that mounts beats a
	// verified-looking one that does not, and the blkx checksum this package
	// writes IS correct, so integrity checking lost nothing.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	buf := make([]byte, kolyBlockSize)
	if _, err := f.ReadAt(buf, fi.Size()-kolyBlockSize); err != nil {
		t.Fatalf("ReadAt koly: %v", err)
	}
	koly, err := parseKoly(buf)
	if err != nil {
		t.Fatalf("parseKoly: %v", err)
	}
	if koly.dataForkChecksum != 0 {
		t.Errorf("dataForkChecksum = 0x%08x, want 0 (type 0, no checksum)", koly.dataForkChecksum)
	}
	if koly.masterChecksum != 0 {
		t.Errorf("masterChecksum = 0x%08x, want 0 (type 0, no checksum)", koly.masterChecksum)
	}
	// …and the blkx table does carry one.
	plist := make([]byte, koly.xmlLength)
	if _, err := f.ReadAt(plist, int64(koly.xmlOffset)); err != nil {
		t.Fatalf("read plist: %v", err)
	}
	items, err := parsePlistBlkx(plist)
	if err != nil {
		t.Fatalf("parsePlistBlkx: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no blkx items")
	}
	tbl, _, err := parseBlkxTable(items[0].Data)
	if err != nil {
		t.Fatalf("parseBlkxTable: %v", err)
	}
	if tbl.checksumType != blkxChecksumCRC32 || tbl.checksum == 0 {
		t.Errorf("blkx checksum type=%d value=0x%08x, want CRC-32 and non-zero", tbl.checksumType, tbl.checksum)
	}
}

func TestReadAllUDIFSectors_ChecksumVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ck.dmg")
	sectors := makeTestSectors(2048)
	if err := writeUDIF(path, sectors, udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}
	// Corrupt a sector byte mid-file (not in plist/koly region).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[10]++ // flip a byte in the data fork
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
}

func TestReadAllUDIFSectors_MultiSegmentError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seg.dmg")
	sectors := makeTestSectors(2048)
	if err := writeUDIF(path, sectors, udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}
	// Patch segmentCount to 2 in the koly block.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	kolyOff := len(raw) - kolyBlockSize
	// segmentCount is at koly[60:64] (big-endian uint32).
	raw[kolyOff+60] = 0
	raw[kolyOff+61] = 0
	raw[kolyOff+62] = 0
	raw[kolyOff+63] = 2
	// Zero out masterChecksum so the segment error fires first, not checksum mismatch.
	raw[kolyOff+360] = 0
	raw[kolyOff+361] = 0
	raw[kolyOff+362] = 0
	raw[kolyOff+363] = 0
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected multi-segment error, got nil")
	}
}

// ─── parseKoly error paths ────────────────────────────────────────────────────

func TestParseKoly_BufferTooSmall(t *testing.T) {
	_, err := parseKoly(make([]byte, 10))
	if err == nil {
		t.Fatal("expected error for small buffer")
	}
}

// ─── parseBlkxTable error paths ───────────────────────────────────────────────

func TestParseBlkxTable_TooSmall(t *testing.T) {
	_, _, err := parseBlkxTable(make([]byte, 10))
	if err == nil {
		t.Fatal("expected error for small buffer")
	}
}

func TestParseBlkxTable_BadMagic(t *testing.T) {
	buf := make([]byte, blkxHeaderSize)
	// wrong magic (all zeros)
	_, _, err := parseBlkxTable(buf)
	if err == nil {
		t.Fatal("expected error for bad blkx magic")
	}
}

func TestParseBlkxTable_TooSmallForRuns(t *testing.T) {
	buf := make([]byte, blkxHeaderSize) // no runs appended
	// set correct magic
	binary.BigEndian.PutUint32(buf[0:4], blkxMagic)
	// claim 100 runs (BlocksRunCount lives at offset 200)
	binary.BigEndian.PutUint32(buf[200:204], 100)
	_, _, err := parseBlkxTable(buf)
	if err == nil {
		t.Fatal("expected error when buffer too small for declared runs")
	}
}

// ─── seekToBlkxArray error path ───────────────────────────────────────────────

func TestSeekToBlkxArray_EOF(t *testing.T) {
	// XML that ends before we find a blkx array
	_, err := parsePlistBlkx([]byte("<plist><dict></dict></plist>"))
	if err == nil {
		t.Fatal("expected error when blkx array not found")
	}
}

func TestSeekToBlkxArray_DecodeKeyError(t *testing.T) {
	// <key> element with mismatched closing tag causes DecodeElement to fail
	xml := `<plist><dict><key>resource-fork</wrong></dict></plist>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from malformed key in seekToBlkxArray")
	}
}

func TestParseBlkxArray_TokenEOF(t *testing.T) {
	// blkx array starts but XML ends before </array> — Token() returns EOF
	xml := `<plist><dict><key>resource-fork</key><dict><key>blkx</key><array>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from truncated blkx array")
	}
}

func TestParseBlkxDictEntry_TokenEOF(t *testing.T) {
	// blkx array contains a <dict> that is never closed — Token() returns EOF
	xml := `<plist><dict><key>resource-fork</key><dict><key>blkx</key><array><dict>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from unclosed dict in blkx array")
	}
}

func TestParseBlkxDictEntry_DecodeKeyError(t *testing.T) {
	// <key> inside dict has mismatched closing tag — DecodeElement fails
	xml := `<plist><dict><key>resource-fork</key><dict><key>blkx</key><array><dict><key>Name</wrong></dict></array></dict></dict></plist>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from malformed key in parseBlkxDictEntry")
	}
}

func TestParseBlkxDictEntry_DecodeStringError(t *testing.T) {
	// <string> inside dict has mismatched closing tag
	xml := `<plist><dict><key>resource-fork</key><dict><key>blkx</key><array><dict><key>Name</key><string>val</wrong></dict></array></dict></dict></plist>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from malformed string in parseBlkxDictEntry")
	}
}

func TestParseBlkxDictEntry_DecodeDataError(t *testing.T) {
	// <data> inside dict has mismatched closing tag
	xml := `<plist><dict><key>resource-fork</key><dict><key>blkx</key><array><dict><key>Data</key><data>abc</wrong></dict></array></dict></dict></plist>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from malformed data in parseBlkxDictEntry")
	}
}

func TestParseBlkxDictEntry_SkipError(t *testing.T) {
	// unknown element inside dict with mismatched closing tag — dec.Skip() fails
	xml := `<plist><dict><key>resource-fork</key><dict><key>blkx</key><array><dict><unknown>val</wrong></dict></array></dict></dict></plist>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error from malformed unknown element in parseBlkxDictEntry")
	}
}

// ─── decompressRun error paths ────────────────────────────────────────────────

func TestDecompressRun_RawReadError(t *testing.T) {
	// ask for 1 sector at offset 1000 but reader only has a few bytes
	run := blkxRun{blockType: blkxRaw, sectorNumber: 0, sectorCount: 1,
		compressedOffset: 0, compressedLength: udifSectorSize}
	_, err := decompressRun(bytes.NewReader([]byte{1, 2, 3}), 0, run)
	if err == nil {
		t.Fatal("expected error reading raw run from short reader")
	}
}

func TestDecompressRun_ZlibBadData(t *testing.T) {
	junk := make([]byte, 16)
	run := blkxRun{blockType: blkxZlib, sectorNumber: 0, sectorCount: 1,
		compressedOffset: 0, compressedLength: uint64(len(junk))}
	_, err := decompressRun(bytes.NewReader(junk), 0, run)
	if err == nil {
		t.Fatal("expected error for invalid zlib data")
	}
}

func TestDecompressRun_ZlibReadError(t *testing.T) {
	run := blkxRun{blockType: blkxZlib, sectorNumber: 0, sectorCount: 1,
		compressedOffset: 1000, compressedLength: 10}
	_, err := decompressRun(bytes.NewReader([]byte{1, 2, 3}), 0, run)
	if err == nil {
		t.Fatal("expected error reading zlib run from short reader")
	}
}

func TestDecompressRun_LzfseSizeMismatch(t *testing.T) {
	// compress 1 sector but claim 2 sectors output
	sectors := makeTestSectors(1)
	compressed, err := lzfse.Compress(sectors)
	if err != nil {
		t.Fatalf("lzfse.Compress: %v", err)
	}
	run := blkxRun{blockType: blkxLzfse, sectorNumber: 0, sectorCount: 2, // claims 2 sectors
		compressedOffset: 0, compressedLength: uint64(len(compressed))}
	_, err = decompressRun(bytes.NewReader(compressed), 0, run)
	if err == nil {
		t.Fatal("expected error for lzfse size mismatch")
	}
	t.Logf("error (expected): %v", err)
}

func TestDecompressRun_LzfseReadError(t *testing.T) {
	run := blkxRun{blockType: blkxLzfse, sectorNumber: 0, sectorCount: 1,
		compressedOffset: 1000, compressedLength: 10}
	_, err := decompressRun(bytes.NewReader([]byte{1, 2, 3}), 0, run)
	if err == nil {
		t.Fatal("expected error reading lzfse run from short reader")
	}
}

// ─── readAllUDIFSectors error paths ───────────────────────────────────────────

func TestReadAllUDIFSectors_PlistReadError(t *testing.T) {
	// Write a koly that claims XML at offset 0 with length 9999 but file is tiny.
	path := filepath.Join(t.TempDir(), "bad.dmg")
	koly := kolyBlock{
		xmlOffset: 0, xmlLength: 9999,
		imageVariant: 1, sectorCount: 0,
		segmentNumber: 1, segmentCount: 1,
	}
	kb := kolyToBytes(koly)
	padding := make([]byte, kolyBlockSize) // just enough to pass size check
	data := append(padding, kb[:]...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error for plist read past end of file")
	}
}

func TestReadAllUDIFSectors_InvalidPlist(t *testing.T) {
	// koly with valid XML offset pointing at garbage bytes
	path := filepath.Join(t.TempDir(), "badplist.dmg")
	garbage := []byte("not xml at all!!!")
	koly := kolyBlock{
		xmlOffset: 0, xmlLength: uint64(len(garbage)),
		imageVariant: 1, sectorCount: 0,
		segmentNumber: 1, segmentCount: 1,
	}
	kb := kolyToBytes(koly)
	data := append(garbage, kb[:]...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error for invalid plist XML")
	}
}

func TestReadAllUDIFSectors_BadBlkxData(t *testing.T) {
	// Write a valid plist that embeds a blkx Data that is not a valid mish table.
	path := filepath.Join(t.TempDir(), "badblkx.dmg")
	badData := make([]byte, blkxHeaderSize) // zero magic = bad
	item := blkxPlistItem{Attributes: "0x0050", Data: badData, ID: "0", Name: "bad"}
	plist := writePlistBlkx([]blkxPlistItem{item})
	koly := kolyBlock{
		xmlOffset: 0, xmlLength: uint64(len(plist)),
		imageVariant: 1, sectorCount: 0,
		segmentNumber: 1, segmentCount: 1,
	}
	kb := kolyToBytes(koly)
	data := append(plist, kb[:]...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error for bad blkx data")
	}
}

// ─── fillSectorsFromRuns error path ──────────────────────────────────────────

func TestFillSectorsFromRuns_DecompressError(t *testing.T) {
	// blkxRaw run pointing beyond reader bounds → decompressRun returns error
	sectors := make([]byte, udifSectorSize)
	run := blkxRun{blockType: blkxRaw, sectorCount: 1, compressedOffset: 1000, compressedLength: udifSectorSize}
	err := fillSectorsFromRuns(bytes.NewReader([]byte{}), blkxTable{}, []blkxRun{run}, sectors)
	if err == nil {
		t.Fatal("expected error from decompressRun in fillSectorsFromRuns")
	}
}

// ─── IsUDIF ───────────────────────────────────────────────────────────────────

func TestIsUDIF_True(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valid.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	if !IsUDIF(path) {
		t.Fatal("expected IsUDIF to return true")
	}
}

func TestIsUDIF_False(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsUDIF(path) {
		t.Fatal("expected IsUDIF to return false for raw file")
	}
}

// ─── UnpackToTemp ─────────────────────────────────────────────────────────────

func TestUnpackToTemp_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	sectors := makeTestSectors(4)
	if err := writeUDIF(path, sectors, udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	tmp, err := UnpackToTemp(path)
	if err != nil {
		t.Fatalf("UnpackToTemp: %v", err)
	}
	defer os.Remove(tmp)
	got, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sectors) {
		t.Fatal("UnpackToTemp: data mismatch")
	}
}

func TestUnpackToTemp_BadSrc(t *testing.T) {
	_, err := UnpackToTemp(filepath.Join(t.TempDir(), "no.dmg"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestUnpackToTemp_CreateTempError(t *testing.T) {
	// Force os.CreateTemp to fail by pointing TMPDIR to a non-existent directory.
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "nonexistent"))
	_, err := UnpackToTemp(path)
	if err == nil {
		t.Fatal("expected error from CreateTemp failure")
	}
}

// ─── PackFromTemp ─────────────────────────────────────────────────────────────

func TestPackFromTemp_Success(t *testing.T) {
	dir := t.TempDir()
	sectors := makeTestSectors(4)
	tmp := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(tmp, sectors, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.dmg")
	if err := PackFromTemp(tmp, dst); err != nil {
		t.Fatalf("PackFromTemp: %v", err)
	}
	recovered, _, err := readAllUDIFSectors(dst)
	if err != nil {
		t.Fatalf("readAllUDIFSectors: %v", err)
	}
	if !bytes.Equal(recovered, sectors) {
		t.Fatal("PackFromTemp: data mismatch")
	}
}

func TestPackFromTemp_BadSrc(t *testing.T) {
	err := PackFromTemp(filepath.Join(t.TempDir(), "no.img"), filepath.Join(t.TempDir(), "out.dmg"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestPackFromTemp_CreateTempError(t *testing.T) {
	// Make destPath's directory read-only so os.CreateTemp inside it fails.
	dir := t.TempDir()
	sectors := makeTestSectors(2)
	tmp := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(tmp, sectors, 0o600); err != nil {
		t.Fatal(err)
	}
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(roDir, "out.dmg")
	if err := PackFromTemp(tmp, dst); err == nil {
		t.Fatal("expected error from CreateTemp in read-only directory")
	}
}

func TestPackFromTemp_RenameFailFallback(t *testing.T) {
	// Force os.Rename to fail by making dst an existing directory.
	// PackFromTemp creates a temp file in filepath.Dir(dst), writes UDIF to it,
	// then tries to rename to dst. If dst is an existing directory, rename fails.
	dir := t.TempDir()
	sectors := makeTestSectors(2)
	tmp := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(tmp, sectors, 0o600); err != nil {
		t.Fatal(err)
	}
	// dst is a directory — Rename(tmpFile, dstDir) returns EISDIR on macOS
	dstDir := filepath.Join(dir, "dstdir")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PackFromTemp(tmp, dstDir); err == nil {
		t.Fatal("expected error from PackFromTemp when dst is an existing directory")
	}
}

// ─── WrapRaw ─────────────────────────────────────────────────────────────────

func TestWrapRaw_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	sectors := makeTestSectors(4)
	if err := os.WriteFile(path, sectors, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WrapRaw(path); err != nil {
		t.Fatalf("WrapRaw: %v", err)
	}
	if !IsUDIF(path) {
		t.Fatal("WrapRaw: expected valid UDIF after wrap")
	}
	recovered, _, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatalf("readAllUDIFSectors after WrapRaw: %v", err)
	}
	if !bytes.Equal(recovered, sectors) {
		t.Fatal("WrapRaw: data mismatch")
	}
}

func TestWrapRaw_BadPath(t *testing.T) {
	err := WrapRaw(filepath.Join(t.TempDir(), "no.img"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ─── ResizeUDRW edge cases ────────────────────────────────────────────────────

func TestResizeUDRW_ZeroSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	if err := ResizeUDRW(path, 0); err == nil {
		t.Fatal("expected error for zero size")
	}
}

func TestResizeUDRW_BadPath(t *testing.T) {
	if err := ResizeUDRW(filepath.Join(t.TempDir(), "no.dmg"), 1024*1024); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ─── ConvertUDIF error path ───────────────────────────────────────────────────

func TestConvertUDIF_BadSrc(t *testing.T) {
	err := ConvertUDIF(filepath.Join(t.TempDir(), "no.dmg"), filepath.Join(t.TempDir(), "out.dmg"), "UDRW")
	if err == nil {
		t.Fatal("expected error for missing src")
	}
}

func TestConvertUDIF_BadDst(t *testing.T) {
	// Create a valid UDRW src but write to an unwritable directory.
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dmg")
	if err := writeUDIF(src, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatalf("writeUDIF: %v", err)
	}
	readOnly := filepath.Join(dir, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(readOnly, "out.dmg")
	if err := ConvertUDIF(src, dst, "UDRW"); err == nil {
		t.Fatal("expected error writing to read-only directory")
	}
}

// ─── dmgCopyFile via Format.ToRaw ────────────────────────────────────────────

func TestFormatToRaw_CopyFallback(t *testing.T) {
	// Force dmgCopyFile by renaming to a cross-device-like path would require
	// two filesystems; instead call dmgCopyFile directly.
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	dst := filepath.Join(dir, "dst.img")
	data := makeTestSectors(2)
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dmgCopyFile(src, dst); err != nil {
		t.Fatalf("dmgCopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("dmgCopyFile: data mismatch")
	}
}

func TestFormatToRaw_RenameFailFallback(t *testing.T) {
	// Force os.Rename to fail by making dst an existing directory.
	// os.Rename(file, dir) returns "file exists" error on macOS,
	// causing ToRaw to fall through to dmgCopyFile (line 53 in format.go).
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dmg")
	if err := writeUDIF(src, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	// dst is an existing directory — Rename will fail, dmgCopyFile will also fail
	dstDir := filepath.Join(dir, "dstdir")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Format{}.ToRaw(src, dstDir, nil)
	if err == nil {
		t.Fatal("expected error from ToRaw when dst is an existing directory")
	}
}

func TestDmgCopyFile_BadSrc(t *testing.T) {
	err := dmgCopyFile(filepath.Join(t.TempDir(), "no.img"), filepath.Join(t.TempDir(), "dst.img"))
	if err == nil {
		t.Fatal("expected error for missing src in dmgCopyFile")
	}
}

func TestDmgCopyFile_BadDst(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.img")
	if err := os.WriteFile(src, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := dmgCopyFile(src, filepath.Join(t.TempDir(), "nodir", "dst.img"))
	if err == nil {
		t.Fatal("expected error for bad dst path in dmgCopyFile")
	}
}

// ─── Format.Resize shrink path ───────────────────────────────────────────────

func TestFormatResize_Shrink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(4096), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	// Shrink to 1 MiB (half)
	if err := (Format{}).Resize(path, 1024*1024); err != nil {
		t.Fatalf("Format.Resize shrink: %v", err)
	}
	sectors, koly, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatalf("readAllUDIFSectors after shrink: %v", err)
	}
	if int64(koly.sectorCount) != 1024*1024/udifSectorSize {
		t.Fatalf("sectorCount = %d, want %d", koly.sectorCount, 1024*1024/udifSectorSize)
	}
	_ = sectors
}

// ─── buildRunsForZlib edge case: data exactly multiple of chunk size ──────────

func TestBuildRunsForZlib_ExactChunkMultiple(t *testing.T) {
	// 4096 sectors = exactly 2 zlib chunks of 2048 each
	sectors := makeTestSectors(4096)
	runs, dataFork := buildRunsForZlib(sectors)
	// 2 data runs + 1 terminator
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	if len(dataFork) == 0 {
		t.Fatal("expected non-empty data fork")
	}
}

// ─── buildRunsForZlib partial chunk ──────────────────────────────────────────

func TestBuildRunsForZlib_PartialChunk(t *testing.T) {
	// 2050 sectors = one full chunk (2048) + one partial (2)
	sectors := makeTestSectors(2050)
	runs, _ := buildRunsForZlib(sectors)
	// 2 data runs + 1 terminator
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	if runs[1].sectorCount != 2 {
		t.Fatalf("second run sectorCount = %d, want 2", runs[1].sectorCount)
	}
}

// ─── decompressRun: zlib decompresses less than expected ─────────────────────

func TestDecompressRun_ZlibShortDecompress(t *testing.T) {
	// Compress 1 sector but claim 2 sectors in the run.
	sectors := makeTestSectors(1)
	compressed := zlibCompress(sectors)
	run := blkxRun{
		blockType: blkxZlib, sectorNumber: 0, sectorCount: 2, // claims 2 sectors
		compressedOffset: 0, compressedLength: uint64(len(compressed)),
	}
	_, err := decompressRun(bytes.NewReader(compressed), 0, run)
	if err == nil {
		t.Fatal("expected error when zlib decompresses fewer bytes than expected")
	}
}

// ─── decompressRun: lzfse decompress error ───────────────────────────────────

func TestDecompressRun_LzfseDecompressError(t *testing.T) {
	// Pass junk bytes as LZFSE data — Decompress should fail.
	junk := []byte("this is not valid lzfse data at all")
	run := blkxRun{
		blockType: blkxLzfse, sectorNumber: 0, sectorCount: 1,
		compressedOffset: 0, compressedLength: uint64(len(junk)),
	}
	_, err := decompressRun(bytes.NewReader(junk), 0, run)
	if err == nil {
		t.Fatal("expected error for invalid lzfse data")
	}
}

// ─── readAllUDIFSectors: file too small ──────────────────────────────────────

func TestReadAllUDIFSectors_FileTooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.img")
	if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error for file too small")
	}
}

// ─── readAllUDIFSectors: bad koly magic ──────────────────────────────────────

func TestReadAllUDIFSectors_BadKolyMagic(t *testing.T) {
	// File large enough to pass size check but has no valid koly magic.
	path := filepath.Join(t.TempDir(), "badmagic.img")
	if err := os.WriteFile(path, make([]byte, kolyBlockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error for bad koly magic")
	}
}

// ─── readAllUDIFSectors: fillSectorsFromRuns error ───────────────────────────

func TestReadAllUDIFSectors_FillSectorsError(t *testing.T) {
	// Construct an image with a RAW run whose compressedOffset points beyond the
	// data fork so that ReadAt fails during sector extraction.
	dir := t.TempDir()
	path := filepath.Join(dir, "patched.dmg")

	const nSectors = 2
	// Build a run that points way past any real data.
	outOfBoundsRun := blkxRun{
		blockType: blkxRaw, sectorNumber: 0, sectorCount: nSectors,
		compressedOffset: 0xFFFFFFFFFFFF00, compressedLength: nSectors * udifSectorSize,
	}
	tbl := blkxTable{sectorNumber: 0, sectorCount: nSectors, dataOffset: 0}
	tblBytes := writeBlkxTable(tbl, []blkxRun{outOfBoundsRun, {blockType: blkxTerm, sectorNumber: nSectors}}, 0)
	plist := writePlistBlkx([]blkxPlistItem{
		{Attributes: "0x0050", Data: tblBytes, ID: "0", Name: "whole disk"},
	})

	xmlOff := uint64(0) // no data fork; plist starts at byte 0
	koly := kolyBlock{
		flags: 1, dataForkOffset: 0, dataForkLength: 0,
		xmlOffset: xmlOff, xmlLength: uint64(len(plist)),
		imageVariant: 1, sectorCount: nSectors,
		segmentNumber: 1, segmentCount: 1,
		masterChecksum: 0, // skip checksum verification
	}
	kb := kolyToBytes(koly)
	var raw []byte
	raw = append(raw, plist...)
	raw = append(raw, kb[:]...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error for out-of-bounds run data")
	}
}

// ─── parseBlkxDictEntry error paths via parsePlistBlkx ───────────────────────

func TestParsePlistBlkx_Base64DecodeError(t *testing.T) {
	// A dict with a Data field that is not valid base64.
	xml := `<?xml version="1.0"?>
<plist><dict><key>resource-fork</key><dict><key>blkx</key><array>
<dict>
<key>Attributes</key><string>0x0050</string>
<key>Data</key><data>!!!not-base64!!!</data>
<key>ID</key><string>0</string>
<key>Name</key><string>test</string>
</dict>
</array></dict></dict></plist>`
	_, err := parsePlistBlkx([]byte(xml))
	if err == nil {
		t.Fatal("expected error for invalid base64 in Data")
	}
}

func TestParsePlistBlkx_UnknownDictKey(t *testing.T) {
	// A dict with an extra unknown element (triggers the default/Skip branch).
	blkxData := writeBlkxTable(
		blkxTable{sectorNumber: 0, sectorCount: 0, dataOffset: 0},
		[]blkxRun{{blockType: blkxTerm}},
		0,
	)
	encoded := base64.StdEncoding.EncodeToString(blkxData)
	xmlStr := `<?xml version="1.0"?>
<plist><dict><key>resource-fork</key><dict><key>blkx</key><array>
<dict>
<key>Attributes</key><string>0x0050</string>
<key>Data</key><data>` + encoded + `</data>
<key>ID</key><string>0</string>
<key>Name</key><string>test</string>
<key>Unknown</key><integer>42</integer>
</dict>
</array></dict></dict></plist>`
	items, err := parsePlistBlkx([]byte(xmlStr))
	if err != nil {
		t.Fatalf("parsePlistBlkx: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
}

// ─── Format.Resize noop ──────────────────────────────────────────────────────

func TestFormatResize_Noop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	const size = 4096
	if err := (Format{}).Create(path, size); err != nil {
		t.Fatal(err)
	}
	// Resize to the same size — should be a noop.
	if err := (Format{}).Resize(path, size); err != nil {
		t.Fatalf("Format.Resize noop: %v", err)
	}
}

// ─── injectable OS function error paths ──────────────────────────────────────

// injectStatError overrides osStatFile to return an error once, then restores.
func injectStatError(t *testing.T) {
	t.Helper()
	orig := osStatFile
	osStatFile = func(f *os.File) (os.FileInfo, error) {
		osStatFile = orig // restore after first call
		return nil, fmt.Errorf("injected stat error")
	}
	t.Cleanup(func() { osStatFile = orig })
}

// injectReadAtError overrides osReadAtFile to return an error once, then restores.
func injectReadAtError(t *testing.T) {
	t.Helper()
	orig := osReadAtFile
	osReadAtFile = func(f *os.File, b []byte, off int64) (int, error) {
		osReadAtFile = orig
		return 0, fmt.Errorf("injected readat error")
	}
	t.Cleanup(func() { osReadAtFile = orig })
}

// injectWriteError overrides osWriteFile to return an error once, then restores.
func injectWriteError(t *testing.T) {
	t.Helper()
	orig := osWriteFile
	osWriteFile = func(f *os.File, b []byte) (int, error) {
		osWriteFile = orig
		return 0, fmt.Errorf("injected write error")
	}
	t.Cleanup(func() { osWriteFile = orig })
}

// injectReadFullError overrides osReadFullFile to return an error once.
func injectReadFullError(t *testing.T) {
	t.Helper()
	orig := osReadFullFile
	osReadFullFile = func(f *os.File, b []byte) (int, error) {
		osReadFullFile = orig
		return 0, fmt.Errorf("injected readfull error")
	}
	t.Cleanup(func() { osReadFullFile = orig })
}

// injectCopyError overrides ioCopyFiles to return an error once.
func injectCopyError(t *testing.T) {
	t.Helper()
	orig := ioCopyFiles
	ioCopyFiles = func(dst *os.File, src *os.File) (int64, error) {
		ioCopyFiles = orig
		return 0, fmt.Errorf("injected copy error")
	}
	t.Cleanup(func() { ioCopyFiles = orig })
}

// injectCloseError overrides osCloseFile to return an error once.
func injectCloseError(t *testing.T) {
	t.Helper()
	orig := osCloseFile
	osCloseFile = func(f *os.File) error {
		osCloseFile = orig
		f.Close() // ensure file is actually closed to avoid leaks
		return fmt.Errorf("injected close error")
	}
	t.Cleanup(func() { osCloseFile = orig })
}

func TestReadAllUDIFSectors_StatError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	injectStatError(t)
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error from injected Stat failure")
	}
}

func TestReadAllUDIFSectors_ReadAtError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	injectReadAtError(t)
	_, _, err := readAllUDIFSectors(path)
	if err == nil {
		t.Fatal("expected error from injected ReadAt failure")
	}
}

func TestDetectUDIFFormat_StatError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	injectStatError(t)
	_, err := DetectUDIFFormat(path)
	if err == nil {
		t.Fatal("expected error from injected Stat failure in DetectUDIFFormat")
	}
}

func TestDetectUDIFFormat_ReadAtError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	injectReadAtError(t)
	_, err := DetectUDIFFormat(path)
	if err == nil {
		t.Fatal("expected error from injected ReadAt failure in DetectUDIFFormat")
	}
}

func TestWriteUDIF_WriteChunkError(t *testing.T) {
	injectWriteError(t)
	err := writeUDIF(filepath.Join(t.TempDir(), "out.dmg"), makeTestSectors(2), udifVariantCodes["UDRW"])
	if err == nil {
		t.Fatal("expected error from injected write failure in writeUDIF (chunk)")
	}
}

func TestWriteUDIF_WriteKolyError(t *testing.T) {
	// Let the first 2 writes (dataFork, plistBytes) succeed, fail on the 3rd (koly).
	orig := osWriteFile
	callCount := 0
	osWriteFile = func(f *os.File, b []byte) (int, error) {
		callCount++
		if callCount >= 3 { // fail on 3rd write (koly trailer)
			osWriteFile = orig
			return 0, fmt.Errorf("injected koly write error")
		}
		return orig(f, b)
	}
	t.Cleanup(func() { osWriteFile = orig })
	err := writeUDIF(filepath.Join(t.TempDir(), "out.dmg"), makeTestSectors(2), udifVariantCodes["UDRW"])
	if err == nil {
		t.Fatal("expected error from injected write failure in writeUDIF (koly)")
	}
}

func TestUnpackToTemp_WriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	injectWriteError(t)
	_, err := UnpackToTemp(path)
	if err == nil {
		t.Fatal("expected error from injected write failure in UnpackToTemp")
	}
}

func TestUnpackToTemp_CloseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := writeUDIF(path, makeTestSectors(2), udifVariantCodes["UDRW"]); err != nil {
		t.Fatal(err)
	}
	injectCloseError(t)
	_, err := UnpackToTemp(path)
	if err == nil {
		t.Fatal("expected error from injected close failure in UnpackToTemp")
	}
}

func TestPackFromTemp_StatError(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(raw, makeTestSectors(2), 0o600); err != nil {
		t.Fatal(err)
	}
	injectStatError(t)
	err := PackFromTemp(raw, filepath.Join(dir, "out.dmg"))
	if err == nil {
		t.Fatal("expected error from injected Stat failure in PackFromTemp")
	}
}

func TestPackFromTemp_ReadFullError(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(raw, makeTestSectors(2), 0o600); err != nil {
		t.Fatal(err)
	}
	injectReadFullError(t)
	err := PackFromTemp(raw, filepath.Join(dir, "out.dmg"))
	if err == nil {
		t.Fatal("expected error from injected ReadFull failure in PackFromTemp")
	}
}

func TestPackFromTemp_WriteUDIFError(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(raw, makeTestSectors(2), 0o600); err != nil {
		t.Fatal(err)
	}
	injectWriteError(t)
	err := PackFromTemp(raw, filepath.Join(dir, "out.dmg"))
	if err == nil {
		t.Fatal("expected error from injected write failure in PackFromTemp (writeUDIF)")
	}
}

func TestWrapRaw_StatError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, makeTestSectors(2), 0o600); err != nil {
		t.Fatal(err)
	}
	injectStatError(t)
	if err := WrapRaw(path); err == nil {
		t.Fatal("expected error from injected Stat failure in WrapRaw")
	}
}

func TestWrapRaw_ReadFullError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, makeTestSectors(2), 0o600); err != nil {
		t.Fatal(err)
	}
	injectReadFullError(t)
	if err := WrapRaw(path); err == nil {
		t.Fatal("expected error from injected ReadFull failure in WrapRaw")
	}
}

func TestDmgCopyFile_CopyError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	dst := filepath.Join(dir, "dst.img")
	if err := os.WriteFile(src, makeTestSectors(2), 0o600); err != nil {
		t.Fatal(err)
	}
	injectCopyError(t)
	if err := dmgCopyFile(src, dst); err == nil {
		t.Fatal("expected error from injected copy failure in dmgCopyFile")
	}
}
