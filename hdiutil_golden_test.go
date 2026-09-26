package dmg

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
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

// TestAFlippedByteInAnAppleImageIsRefused.
//
// Before the checksums were enforced this was silent, and silence is the whole
// point of the case: one flipped byte in a zlib run gave back a whole image of
// the wrong bytes with no error at all, measured on a 900 KiB ISO whose SHA-256
// changed while its length did not. zlib's own Adler-32 does not save it either,
// because io.ReadFull stops at the output length and never reaches the trailer.
//
// Each flavour gets its own subtest because the four codecs fail differently:
// bzip2 and zlib carry internal checks that only fire if the stream is read to
// the end, and ADC and LZFSE carry none at all -- so for two of the four the
// koly's CRC-32 over the compressed fork is the ONLY thing that can notice.
func TestAFlippedByteInAnAppleImageIsRefused(t *testing.T) {
	for _, format := range []string{"UDCO", "UDZO", "UDBZ", "ULFO"} {
		t.Run(format, func(t *testing.T) {
			img, err := appleImages.ReadFile("testdata/hdiutil-" + format + ".dmg")
			if err != nil {
				t.Fatal(err)
			}

			// Premise: intact, this image reads. Without this the subtest would
			// pass on a fixture that never worked in the first place.
			good := filepath.Join(t.TempDir(), "good.dmg")
			if err := os.WriteFile(good, img, 0o600); err != nil {
				t.Fatal(err)
			}
			tmp, err := UnpackToTemp(good)
			if err != nil {
				t.Fatalf("the intact fixture does not read: %v", err)
			}
			os.Remove(tmp)

			// One byte, inside the compressed fork: past the first sector and a
			// long way from both the plist and the koly trailer.
			damaged := append([]byte(nil), img...)
			damaged[600] ^= 0xFF
			bad := filepath.Join(t.TempDir(), "bad.dmg")
			if err := os.WriteFile(bad, damaged, 0o600); err != nil {
				t.Fatal(err)
			}

			tmp, err = UnpackToTemp(bad)
			if err == nil {
				os.Remove(tmp)
				t.Fatal("a flipped byte was accepted: the image came back with no " +
					"error and the wrong bytes in it")
			}
			if !strings.Contains(err.Error(), "checksum") {
				t.Errorf("err = %v, want a checksum mismatch: something else caught "+
					"this, so the checksums are still not what is refusing it", err)
			}
			t.Logf("refused: %v", err)
		})
	}
}

// TestTheDataForkChecksumIsWhatCatchesACodecWithNoInternalCheck.
//
// ADC and LZFSE have no checksum of their own, so for those two flavours the
// refusal above can only be coming from the koly. This removes the blkx check's
// reach by damaging an image and asserting the error names the DATA FORK, which
// is checked first and before anything is decoded.
func TestTheDataForkChecksumIsWhatCatchesACodecWithNoInternalCheck(t *testing.T) {
	for _, format := range []string{"UDCO", "ULFO"} {
		t.Run(format, func(t *testing.T) {
			img, err := appleImages.ReadFile("testdata/hdiutil-" + format + ".dmg")
			if err != nil {
				t.Fatal(err)
			}
			damaged := append([]byte(nil), img...)
			damaged[600] ^= 0xFF
			path := filepath.Join(t.TempDir(), "bad.dmg")
			if err := os.WriteFile(path, damaged, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = UnpackToTemp(path)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "data fork") {
				t.Errorf("err = %v, want the data fork check to be what refuses it: "+
					"%s carries no internal checksum, so nothing else can", err, format)
			}
		})
	}
}

// TestTheBlkxChecksumCatchesADecoderThatDisagreesWithTheImage.
//
// The data fork check covers damage to the STORED bytes and fires first, so it
// can never leave the blkx check anything to do. The blkx check answers a
// different question: the compressed bytes are intact and the sectors that came
// out of them are not what the image says they should be. That is a DECODER
// fault, and it is not hypothetical here -- the flavour-reading defect this
// file's other tests exist for produced exactly that, and against an image
// carrying a real checksum it would have been a loud refusal rather than wrong
// bytes.
//
// It is witnessed by rewriting the table's declared checksum inside the plist,
// which sits OUTSIDE the data fork: the fork's own CRC still agrees, so the
// first check passes and the second one has to be what refuses.
func TestTheBlkxChecksumCatchesADecoderThatDisagreesWithTheImage(t *testing.T) {
	img, err := appleImages.ReadFile("testdata/hdiutil-UDZO.dmg")
	if err != nil {
		t.Fatal(err)
	}
	patched, before, after := withBlkxChecksum(t, img, 0xDEADBEEF)
	if before == after {
		t.Fatalf("the declared checksum was already 0x%08x: this would test nothing", after)
	}
	t.Logf("declared checksum rewritten from 0x%08x to 0x%08x", before, after)

	path := filepath.Join(t.TempDir(), "wrongsum.dmg")
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = UnpackToTemp(path)
	if err == nil {
		t.Fatal("an image declaring a checksum its own sectors do not match was accepted")
	}
	if strings.Contains(err.Error(), "data fork") {
		t.Fatalf("the DATA FORK check refused this (%v), so the blkx check is still "+
			"unwitnessed: the plist is outside the fork and must not have moved it", err)
	}
	if !strings.Contains(err.Error(), "blkx checksum mismatch") {
		t.Errorf("err = %v, want the blkx check to be what refuses it", err)
	}
	t.Logf("refused: %v", err)
}

// withBlkxChecksum rewrites the first blkx table's declared CRC-32 in place,
// returning the new image along with the old and new values.
//
// The base64 payload keeps its length because four bytes are replaced by four
// bytes, so every offset in the koly still points where it did. That matters:
// the point of the case is that only ONE field changed.
func withBlkxChecksum(t *testing.T, img []byte, want uint32) (out []byte, before, after uint32) {
	t.Helper()
	koly, err := parseKoly(img[len(img)-kolyBlockSize:])
	if err != nil {
		t.Fatalf("parseKoly: %v", err)
	}
	xml := img[koly.xmlOffset : koly.xmlOffset+koly.xmlLength]
	items, err := parsePlistBlkx(xml)
	if err != nil {
		t.Fatalf("parsePlistBlkx: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no blkx table in the plist")
	}
	mish := items[0].Data
	before = binary.BigEndian.Uint32(mish[72:76])
	patchedMish := append([]byte(nil), mish...)
	binary.BigEndian.PutUint32(patchedMish[72:76], want)

	oldB64 := base64.StdEncoding.EncodeToString(mish)
	newB64 := base64.StdEncoding.EncodeToString(patchedMish)
	if len(oldB64) != len(newB64) {
		t.Fatalf("base64 length changed from %d to %d: every offset after it would move",
			len(oldB64), len(newB64))
	}
	// The plist wraps its base64 across lines, so the encoded form in the file is
	// not the single string encoding gives back; replaceBase64InPlist matches it
	// with the whitespace stripped and writes back to the original positions.
	out = append([]byte(nil), img...)
	if !replaceBase64InPlist(out[koly.xmlOffset:koly.xmlOffset+koly.xmlLength], mish, patchedMish) {
		t.Fatal("the table's base64 was not found in the plist to patch")
	}
	return out, before, want
}

// replaceBase64InPlist rewrites one blkx table's base64 inside the plist,
// tolerating the line breaks the plist writes it with.
func replaceBase64InPlist(plist, old, new []byte) bool {
	oldB64 := base64.StdEncoding.EncodeToString(old)
	newB64 := base64.StdEncoding.EncodeToString(new)
	// Walk the plist stripping whitespace, and record where each kept byte came
	// from, so a match in the stripped form can be written back in place.
	var stripped []byte
	var at []int
	for i, c := range plist {
		switch c {
		case ' ', '\t', '\n', '\r':
		default:
			stripped = append(stripped, c)
			at = append(at, i)
		}
	}
	idx := bytes.Index(stripped, []byte(oldB64))
	if idx < 0 {
		return false
	}
	for j := range oldB64 {
		plist[at[idx+j]] = newB64[j]
	}
	return true
}

// errAtReader fails every read, so a checksum that cannot read its bytes has
// something to report.
type errAtReader struct{ err error }

func (e errAtReader) ReadAt([]byte, int64) (int, error) { return 0, e.err }

// TestVerifyDataForkReportsAReadFailureRatherThanAMismatch.
//
// A checksum that cannot read the bytes it is meant to check must say so. If it
// reported a mismatch instead, a failing disk would read as a corrupt image and
// the person holding it would go looking for the wrong problem.
//
// The reader is injected because readAllUDIFSectors opens the file itself, so
// there is no path through it that can fail this way on demand. What this proves
// is PROPAGATION, not that a real read ever fails here.
func TestVerifyDataForkReportsAReadFailureRatherThanAMismatch(t *testing.T) {
	boom := errors.New("the disk said no")
	err := verifyDataFork(errAtReader{boom}, kolyBlock{
		dataForkOffset: 0, dataForkLength: 512, dataForkChecksum: 0x12345678,
	})
	if err == nil {
		t.Fatal("a data fork that could not be read was reported as checking out")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
	if strings.Contains(err.Error(), "mismatch") {
		t.Errorf("err = %v, want a read failure rather than a mismatch: the bytes "+
			"were never seen, so nothing can be said about whether they agree", err)
	}
}

// TestVerifyDataForkSkipsAnEmptySlot. This package's own writer leaves the
// checksum slots at zero, and an image with nothing declared has nothing to
// check -- as distinct from an image declaring zero, which no writer does.
func TestVerifyDataForkSkipsAnEmptySlot(t *testing.T) {
	if err := verifyDataFork(errAtReader{errors.New("must not be read")}, kolyBlock{
		dataForkOffset: 0, dataForkLength: 512, dataForkChecksum: 0,
	}); err != nil {
		t.Errorf("an empty checksum slot was checked anyway: %v", err)
	}
	if err := verifyDataFork(errAtReader{errors.New("must not be read")}, kolyBlock{
		dataForkOffset: 0, dataForkLength: 0, dataForkChecksum: 0x12345678,
	}); err != nil {
		t.Errorf("a zero-length fork was checked anyway: %v", err)
	}
}

// TestABlkxTableIsCheckedOverItsOwnSectorsNotTheWholeImage.
//
// Every image hdiutil writes from a whole disk carries exactly ONE blkx table
// covering every sector, so "this table's sectors" and "the whole image" are the
// same span and no corpus of real images can tell the two rules apart. An
// ablation replacing the table's span with the whole image passed the entire
// suite. A partitioned image has one table per partition, and authoring one here
// would mean attaching a block device, which this machine does not do.
//
// So the image is built rather than converted: two RAW tables, sectors 0..4 and
// 4..8, each declaring a correct CRC-32 over ITS OWN four sectors and nothing
// else. The two halves hold different bytes, so a check over the whole image
// disagrees with both declarations. Its own output is the right fixture here
// because the property under test is this package's span arithmetic; Apple's
// four images keep judging the format itself.
func TestABlkxTableIsCheckedOverItsOwnSectorsNotTheWholeImage(t *testing.T) {
	const half = 4 // sectors per table

	first := bytes.Repeat([]byte{0xA1}, half*udifSectorSize)
	second := bytes.Repeat([]byte{0xB2}, half*udifSectorSize)
	fork := append(append([]byte(nil), first...), second...)

	// Premise: the halves differ, so a CRC over both cannot equal either.
	sumFirst := crc32.ChecksumIEEE(first)
	sumSecond := crc32.ChecksumIEEE(second)
	sumBoth := crc32.ChecksumIEEE(fork)
	if sumFirst == sumSecond || sumBoth == sumFirst || sumBoth == sumSecond {
		t.Fatalf("the spans are not distinguishable by checksum (first 0x%08x, "+
			"second 0x%08x, both 0x%08x): this fixture would prove nothing",
			sumFirst, sumSecond, sumBoth)
	}

	var items []blkxPlistItem
	for i, part := range [][2]uint64{{0, half}, {half, half}} {
		tbl := blkxTable{
			sectorNumber: part[0],
			sectorCount:  part[1],
			dataOffset:   0,
			checksumType: blkxChecksumCRC32,
		}
		sum := sumFirst
		if i == 1 {
			sum = sumSecond
		}
		runs := []blkxRun{
			{
				blockType:        blkxRaw,
				sectorNumber:     0, // relative to the table, as the format has it
				sectorCount:      part[1],
				compressedOffset: part[0] * udifSectorSize,
				compressedLength: part[1] * udifSectorSize,
			},
			{blockType: blkxTerm, sectorNumber: part[1]},
		}
		items = append(items, blkxPlistItem{
			Attributes: "0x0050",
			ID:         fmt.Sprint(i),
			Name:       fmt.Sprintf("part %d", i),
			Data:       writeBlkxTable(tbl, runs, sum),
		})
	}

	plist := writePlistBlkx(items)
	koly := kolyBlock{
		dataForkOffset: 0,
		dataForkLength: uint64(len(fork)),
		xmlOffset:      uint64(len(fork)),
		xmlLength:      uint64(len(plist)),
		imageVariant:   2,
		sectorCount:    2 * half,
		segmentNumber:  1,
		segmentCount:   1,
	}
	kb := kolyToBytes(koly)
	img := append(append(append([]byte(nil), fork...), plist...), kb[:]...)

	path := filepath.Join(t.TempDir(), "twotables.dmg")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}

	sectors, _, err := readAllUDIFSectors(path)
	if err != nil {
		t.Fatalf("a two-table image whose every declaration is correct was refused: %v", err)
	}
	if !bytes.Equal(sectors, fork) {
		t.Errorf("sectors differ from what went in at byte %d", firstDifference(sectors, fork))
	}
}

// One more embedded image: a UDZO whose blkx table MIXES runs that store data
// with runs that only zero-fill.
//
// This is the case none of the other fixtures can reach. hdiutil emits a
// zero-fill run only when it recognises the filesystem inside the image and can
// see its unused space, so converting a raw file never produces one however large
// or however compressible it is -- measured: a 40 MiB image of alternating zero,
// pattern and random megabytes came out as 27 zlib runs and 13 raw runs and not
// one zero-fill.
//
// The container is hdiutil's, which is the part under test. The HFS+ volume inside
// it was written by go-filesystems/hfsplus, because authoring one with hdiutil
// means attaching a block device; `hdiutil imageinfo` names its partition
// "whole disk (Apple_HFS : 0)", so Apple's own tool agrees about what is there.
//
// No raw counterpart is embedded, and none is needed: Apple's declared checksum
// IS the assertion about the bytes, and it is not ours.
//
//go:embed testdata/hdiutil-zerofill-UDZO.dmg
var zeroFillImage embed.FS

// TestABlkxChecksumSkipsTheRunsThatOnlyZeroFill.
//
// The rule is not "CRC-32 of the sectors this table describes". It is "CRC-32 of
// the bytes this table's runs produced", and a zero-fill run produces none --
// not even zeros standing in for its sectors.
//
// This fixture is the tenth image, and the one that overturned the other nine: a
// check enforced on the span rule refused it, citing a mismatch, while nothing
// about it is wrong.
func TestABlkxChecksumSkipsTheRunsThatOnlyZeroFill(t *testing.T) {
	img, err := zeroFillImage.ReadFile("testdata/hdiutil-zerofill-UDZO.dmg")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "zerofill.dmg")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}

	// Premise, in two halves. Without both, this is just another image that reads.
	tbl, runs := onlyBlkxTable(t, img)
	if tbl.checksumType != blkxChecksumCRC32 || tbl.checksum == 0 {
		t.Fatalf("the table declares no CRC-32 (type %d, value 0x%08x): there would "+
			"be nothing here to get wrong", tbl.checksumType, tbl.checksum)
	}
	var producing, zeroFill int
	for _, r := range runs {
		switch {
		case r.blockType == blkxTerm:
		case runProducesData(r.blockType):
			producing++
		default:
			zeroFill++
		}
	}
	if producing == 0 || zeroFill == 0 {
		t.Fatalf("the table has %d producing runs and %d zero-fill runs: it takes "+
			"both for the two rules to give different answers", producing, zeroFill)
	}
	t.Logf("table: %d producing runs, %d zero-fill runs, declares 0x%08x",
		producing, zeroFill, tbl.checksum)

	// Accepting it is the assertion: Apple declared that value over its own reading
	// of these sectors, so agreeing with it is agreeing about every byte.
	got, err := UnpackToTemp(path)
	if err != nil {
		t.Fatalf("UnpackToTemp: %v", err)
	}
	defer os.Remove(got)
	sectors, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}

	// And the premise that matters most: the two rules really do disagree here, so
	// this fixture can tell them apart.
	lo := tbl.sectorNumber * udifSectorSize
	hi := lo + tbl.sectorCount*udifSectorSize
	if hi > uint64(len(sectors)) {
		t.Fatalf("the table describes sectors past the end of the image (%d > %d)", hi, len(sectors))
	}
	if spanRule := crc32.ChecksumIEEE(sectors[lo:hi]); spanRule == tbl.checksum {
		t.Fatalf("a CRC over the whole span also gives 0x%08x, so this fixture "+
			"cannot tell the two rules apart", spanRule)
	} else {
		t.Logf("a CRC over the whole span would be 0x%08x, which is the wrong answer", spanRule)
	}
}

// onlyBlkxTable returns the single blkx table of an image that has one.
func onlyBlkxTable(t *testing.T, img []byte) (blkxTable, []blkxRun) {
	t.Helper()
	koly, err := parseKoly(img[len(img)-kolyBlockSize:])
	if err != nil {
		t.Fatalf("parseKoly: %v", err)
	}
	items, err := parsePlistBlkx(img[koly.xmlOffset : koly.xmlOffset+koly.xmlLength])
	if err != nil {
		t.Fatalf("parsePlistBlkx: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("%d blkx tables, want 1", len(items))
	}
	tbl, runs, err := parseBlkxTable(items[0].Data)
	if err != nil {
		t.Fatalf("parseBlkxTable: %v", err)
	}
	return tbl, runs
}

// TestABlkxTableWithNoChecksumIsNotJudged. This package's own writer used to
// leave the slot at type 0, and an older image may still. Nothing declared means
// nothing to compare against -- as distinct from a declared zero, which no writer
// produces.
func TestABlkxTableWithNoChecksumIsNotJudged(t *testing.T) {
	for _, tbl := range []blkxTable{
		{checksumType: 0, checksum: 0},
		{checksumType: 0, checksum: 0x12345678},        // a value under no type
		{checksumType: blkxChecksumCRC32, checksum: 0}, // a type with no value
	} {
		if err := verifyBlkxChecksum(tbl, 0xFFFFFFFF); err != nil {
			t.Errorf("type %d value 0x%08x was judged anyway: %v",
				tbl.checksumType, tbl.checksum, err)
		}
	}
}

// TestProducedCRCStopsAtTheEndOfTheImage.
//
// A run may declare more sectors than the image holds, and a checksum must not
// read past the buffer to find out. This asserts the clamp by computing the same
// value two ways: an overlong run over a short image, and an exact run over the
// same bytes.
func TestProducedCRCStopsAtTheEndOfTheImage(t *testing.T) {
	sectors := bytes.Repeat([]byte{0x5A}, 2*udifSectorSize)

	overlong := []blkxRun{{blockType: blkxRaw, sectorNumber: 0, sectorCount: 99}}
	exact := []blkxRun{{blockType: blkxRaw, sectorNumber: 0, sectorCount: 2}}
	if got, want := producedCRC(sectors, overlong), producedCRC(sectors, exact); got != want {
		t.Errorf("an overlong run gave 0x%08x, want 0x%08x -- the same bytes are all "+
			"there are", got, want)
	}
	if got, want := producedCRC(sectors, exact), crc32.ChecksumIEEE(sectors); got != want {
		t.Errorf("producedCRC = 0x%08x over the whole image, want 0x%08x", got, want)
	}
}
