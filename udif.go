package dmg

// Pure-Go Apple UDIF disk image helpers: DetectUDIFFormat, ConvertUDIF, ResizeUDRW.
// No external tools (hdiutil, plutil) are required.
//
// Koly block layout (512 bytes, big-endian, at end of file):
//   [0:4]   magic 'koly'   [4:12]  version+hdrSize
//   [12:16] flags           [16:24] runningDataForkOffset
//   [24:32] dataForkOffset  [32:40] dataForkLength
//   [40:56] rsrcFork off+len [56:64] segNumber+segCount
//   [64:80] segmentID(UUID) [80:216] dataForkChecksum(type+size+data)
//   [216:232] xmlOffset+xmlLength [232:352] reserved
//   [352:488] masterChecksum(type+size+data)
//   [488:492] imageVariant  [492:500] sectorCount  [500:512] reserved2

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/go-compressions/lzfse"
)

// osStatFile is the function used to stat an open file. Overridable in tests.
var osStatFile = func(f *os.File) (os.FileInfo, error) { return f.Stat() }

// osReadAtFile is the function used to read from an open file. Overridable in tests.
var osReadAtFile = func(f *os.File, b []byte, off int64) (int, error) { return f.ReadAt(b, off) }

// osWriteFile is the function used to write to an open file. Overridable in tests.
var osWriteFile = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// osReadFullFile is the function used to read from an open file. Overridable in tests.
var osReadFullFile = func(f *os.File, b []byte) (int, error) { return io.ReadFull(f, b) }

// ioCopyFiles is the function used to copy between files. Overridable in tests.
var ioCopyFiles = func(dst *os.File, src *os.File) (int64, error) { return io.Copy(dst, src) }

// osCloseFile is the function used to close a file. Overridable in tests.
var osCloseFile = func(f *os.File) error { return f.Close() }

// ─── constants & variant maps ─────────────────────────────────────────────────

const (
	kolyMagic      = uint32(0x6B6F6C79) // 'koly'
	kolyBlockSize  = 512
	udifSectorSize = 512
	blkxMagic      = uint32(0x6D697368) // 'mish'
	// blkxHeaderSize is the size of the UDIF BLKXTable (mish) header that
	// precedes the 40-byte chunk-run entries. The Apple/UDIF layout is:
	//   [0:4]    Signature 'mish'        [4:8]   Version (1)
	//   [8:16]   SectorNumber            [16:24] SectorCount
	//   [24:32]  DataOffset              [32:36] BuffersNeeded
	//   [36:40]  BlockDescriptors        [40:64] reserved (6×uint32)
	//   [64:200] UDIFChecksum: type[64:68] + size[68:72] + data[72:200]
	//   [200:204] BlocksRunCount
	//   [204:…]  run entries (40 bytes each)
	// This MUST be 204 (not 200): qemu-img's block/dmg.c reads the run count
	// at +200 and the first chunk at +204; emitting a 200-byte header shifts
	// every chunk field by 4 bytes and makes the image unreadable.
	blkxHeaderSize = 204

	blkxNocopy = uint32(0x00000000) // zero-fill, no stored data
	blkxRaw    = uint32(0x00000001) // raw / uncompressed
	blkxIgnore = uint32(0x00000002) // zero-fill, stored length may be non-zero
	blkxFree   = uint32(0x7FFFFFFE) // unallocated
	blkxLzfse  = uint32(0x80000004) // LZFSE compressed (Apple, macOS 10.12+)
	blkxZlib   = uint32(0x80000005) // zlib compressed
	blkxTerm   = uint32(0xFFFFFFFF) // terminator

	blkxChecksumCRC32 = uint32(0x00000002)
)

// imageVariant (koly +488) is a LAYOUT discriminator, not a format selector.
// Measured on macOS 26 with everything else held constant:
//
//	imageVariant = 1, one whole-disk blkx  -> attach fails, "Bad file descriptor"
//	imageVariant = 2, one whole-disk blkx  -> attaches
//
// 1 means the blkx array describes a PARTITION MAP; with 1 and no partition
// entries hdiutil goes looking for partitions and dies. libdmg-hfsplus names
// the two kUDIFPartitionImageType / kUDIFDeviceImageType, which reads backwards
// from the observed behaviour -- trust the behaviour, not the names.
//
// The compression format is nowhere in the koly block: a reader infers it from
// the chunk types in the blkx table, which is what DetectUDIFFormat now does.
const (
	imageVariantPartitioned = uint32(1)
	imageVariantWholeDisk   = uint32(2)
)

var udifVariantCodes = map[string]uint32{
	"UDRW": 1, "UDRO": 2, "UDCO": 3, "UDZO": 4, "UDBZ": 5, "UDSP": 11,
}

// ─── koly block ───────────────────────────────────────────────────────────────

type kolyBlock struct {
	flags                 uint32
	runningDataForkOffset uint64
	dataForkOffset        uint64
	dataForkLength        uint64
	rsrcForkOffset        uint64
	rsrcForkLength        uint64
	segmentNumber         uint32
	segmentCount          uint32
	segmentID             [16]byte
	xmlOffset             uint64
	xmlLength             uint64
	imageVariant          uint32
	sectorCount           uint64
	// CRC-32 checksums (type 0x00000002)
	dataForkChecksum uint32 // CRC-32 of the data fork bytes
	masterChecksum   uint32 // CRC-32 of all uncompressed sector bytes
}

func parseKoly(b []byte) (kolyBlock, error) {
	if len(b) < kolyBlockSize {
		return kolyBlock{}, fmt.Errorf("udif: koly buffer too small")
	}
	if binary.BigEndian.Uint32(b[0:4]) != kolyMagic {
		return kolyBlock{}, fmt.Errorf("udif: bad koly magic %08x", binary.BigEndian.Uint32(b[0:4]))
	}
	k := kolyBlock{
		flags:                 binary.BigEndian.Uint32(b[12:16]),
		runningDataForkOffset: binary.BigEndian.Uint64(b[16:24]),
		dataForkOffset:        binary.BigEndian.Uint64(b[24:32]),
		dataForkLength:        binary.BigEndian.Uint64(b[32:40]),
		rsrcForkOffset:        binary.BigEndian.Uint64(b[40:48]),
		rsrcForkLength:        binary.BigEndian.Uint64(b[48:56]),
		segmentNumber:         binary.BigEndian.Uint32(b[56:60]),
		segmentCount:          binary.BigEndian.Uint32(b[60:64]),
		xmlOffset:             binary.BigEndian.Uint64(b[216:224]),
		xmlLength:             binary.BigEndian.Uint64(b[224:232]),
		imageVariant:          binary.BigEndian.Uint32(b[488:492]),
		sectorCount:           binary.BigEndian.Uint64(b[492:500]),
	}
	copy(k.segmentID[:], b[64:80])
	// dataForkChecksum at [80:216]: type(4)+size(4)+crc(4)+zeros
	k.dataForkChecksum = binary.BigEndian.Uint32(b[88:92])
	// masterChecksum at [352:488]: type(4)+size(4)+crc(4)+zeros
	k.masterChecksum = binary.BigEndian.Uint32(b[360:364])
	return k, nil
}

func kolyToBytes(k kolyBlock) [kolyBlockSize]byte {
	var b [kolyBlockSize]byte
	binary.BigEndian.PutUint32(b[0:4], kolyMagic)
	binary.BigEndian.PutUint32(b[4:8], 4)
	binary.BigEndian.PutUint32(b[8:12], kolyBlockSize)
	binary.BigEndian.PutUint32(b[12:16], k.flags)
	binary.BigEndian.PutUint64(b[16:24], k.runningDataForkOffset)
	binary.BigEndian.PutUint64(b[24:32], k.dataForkOffset)
	binary.BigEndian.PutUint64(b[32:40], k.dataForkLength)
	binary.BigEndian.PutUint64(b[40:48], k.rsrcForkOffset)
	binary.BigEndian.PutUint64(b[48:56], k.rsrcForkLength)
	binary.BigEndian.PutUint32(b[56:60], k.segmentNumber)
	binary.BigEndian.PutUint32(b[60:64], k.segmentCount)
	copy(b[64:80], k.segmentID[:])
	// dataForkChecksum: [80:84]=type, [84:88]=size in bits, [88:92]=value.
	// Type 0 means "no checksum", and a zero value must be written that way:
	// declaring type 2 makes hdiutil validate the field, so an unset value
	// there is not "unverified", it is "wrong" and the image will not attach.
	if k.dataForkChecksum != 0 {
		binary.BigEndian.PutUint32(b[80:84], 0x00000002)
		binary.BigEndian.PutUint32(b[84:88], 0x00000020)
		binary.BigEndian.PutUint32(b[88:92], k.dataForkChecksum)
	}
	binary.BigEndian.PutUint64(b[216:224], k.xmlOffset)
	binary.BigEndian.PutUint64(b[224:232], k.xmlLength)
	// masterChecksum: same encoding, same reason.
	if k.masterChecksum != 0 {
		binary.BigEndian.PutUint32(b[352:356], 0x00000002)
		binary.BigEndian.PutUint32(b[356:360], 0x00000020)
		binary.BigEndian.PutUint32(b[360:364], k.masterChecksum)
	}
	binary.BigEndian.PutUint32(b[488:492], k.imageVariant)
	binary.BigEndian.PutUint64(b[492:500], k.sectorCount)
	return b
}

// ─── blkx table ───────────────────────────────────────────────────────────────

type blkxTable struct {
	sectorNumber uint64
	sectorCount  uint64
	dataOffset   uint64
	// checksum is the blkx table's own CRC-32 of the UNCOMPRESSED sectors it
	// describes (header +64 type, +68 size, +72 value). This is the checksum
	// this package can compute correctly, and the one qemu-img validates, so
	// it is what integrity checking hangs off -- not the koly block, whose
	// value hdiutil rejects (see kolyToBytes).
	checksumType uint32
	checksum     uint32
}

type blkxRun struct {
	blockType        uint32
	sectorNumber     uint64 // relative to blkxTable.sectorNumber
	sectorCount      uint64
	compressedOffset uint64 // relative to blkxTable.dataOffset
	compressedLength uint64
}

func parseBlkxTable(b []byte) (blkxTable, []blkxRun, error) {
	if len(b) < blkxHeaderSize {
		return blkxTable{}, nil, fmt.Errorf("blkx: buffer too small (%d)", len(b))
	}
	if binary.BigEndian.Uint32(b[0:4]) != blkxMagic {
		return blkxTable{}, nil, fmt.Errorf("blkx: bad magic")
	}
	n := int(binary.BigEndian.Uint32(b[200:204])) // BlocksRunCount
	if len(b) < blkxHeaderSize+n*40 {
		return blkxTable{}, nil, fmt.Errorf("blkx: buffer too small for %d runs", n)
	}
	t := blkxTable{
		checksumType: binary.BigEndian.Uint32(b[64:68]),
		checksum:     binary.BigEndian.Uint32(b[72:76]),
		sectorNumber: binary.BigEndian.Uint64(b[8:16]),
		sectorCount:  binary.BigEndian.Uint64(b[16:24]),
		dataOffset:   binary.BigEndian.Uint64(b[24:32]),
	}
	runs := make([]blkxRun, n)
	for i := range runs {
		o := blkxHeaderSize + i*40
		runs[i] = blkxRun{
			blockType:        binary.BigEndian.Uint32(b[o : o+4]),
			sectorNumber:     binary.BigEndian.Uint64(b[o+8 : o+16]),
			sectorCount:      binary.BigEndian.Uint64(b[o+16 : o+24]),
			compressedOffset: binary.BigEndian.Uint64(b[o+24 : o+32]),
			compressedLength: binary.BigEndian.Uint64(b[o+32 : o+40]),
		}
	}
	return t, runs, nil
}

// writeBlkxTable serialises a mish table. checksum is the CRC-32 of the
// uncompressed sector data covered by this table.
func writeBlkxTable(t blkxTable, runs []blkxRun, checksum uint32) []byte {
	buf := make([]byte, blkxHeaderSize+len(runs)*40)
	binary.BigEndian.PutUint32(buf[0:4], blkxMagic)
	binary.BigEndian.PutUint32(buf[4:8], 1) // version
	binary.BigEndian.PutUint64(buf[8:16], t.sectorNumber)
	binary.BigEndian.PutUint64(buf[16:24], t.sectorCount)
	binary.BigEndian.PutUint64(buf[24:32], t.dataOffset)
	// BuffersNeeded at [32:36] is how many 512-byte sectors a reader must be
	// able to hold to decompress the largest run. Leaving it ZERO is what made
	// every compressed image this package wrote fail to attach with "corrupt
	// image" -- measured on macOS 26, sweeping only this field on an otherwise
	// untouched UDZO whose largest run is 2048 sectors:
	//
	//	1 -> corrupt image     2047 -> corrupt image
	//	8 -> corrupt image     2048 -> ATTACHES        2056 -> ATTACHES
	//
	// A sharp boundary at the largest run, so the value is derived from the
	// runs rather than hardcoded. The +8 matches what hdiutil writes (2056 for
	// 2048-sector chunks) and costs a reader nothing.
	//
	// The raw path survived this because a raw run needs no decompression
	// buffer at all, which is why the defect only ever showed on UDZO.
	var largest uint64
	for _, r := range runs {
		if r.blockType != blkxTerm && r.sectorCount > largest {
			largest = r.sectorCount
		}
	}
	if largest > 0 {
		binary.BigEndian.PutUint32(buf[32:36], uint32(largest)+8)
	}
	// UDIFChecksum at [64:200]: type[64:68]=CRC-32, size[68:72]=32 bits, data[72:76]=value
	binary.BigEndian.PutUint32(buf[64:68], 0x00000002)
	binary.BigEndian.PutUint32(buf[68:72], 0x00000020)
	binary.BigEndian.PutUint32(buf[72:76], checksum)
	// BlocksRunCount at [200:204]; chunk-run entries follow at [204:].
	binary.BigEndian.PutUint32(buf[200:204], uint32(len(runs)))
	for i, r := range runs {
		o := blkxHeaderSize + i*40
		binary.BigEndian.PutUint32(buf[o:o+4], r.blockType)
		binary.BigEndian.PutUint64(buf[o+8:o+16], r.sectorNumber)
		binary.BigEndian.PutUint64(buf[o+16:o+24], r.sectorCount)
		binary.BigEndian.PutUint64(buf[o+24:o+32], r.compressedOffset)
		binary.BigEndian.PutUint64(buf[o+32:o+40], r.compressedLength)
	}
	return buf
}

// ─── sector decompression ─────────────────────────────────────────────────────

// decompressRun returns sectorCount*udifSectorSize bytes for one blkx run.
// The file (f) is used as the backing ReaderAt; dataOffset is tbl.dataOffset.
func decompressRun(f io.ReaderAt, dataOffset uint64, r blkxRun) ([]byte, error) {
	out := make([]byte, r.sectorCount*udifSectorSize)
	switch r.blockType {
	case blkxNocopy, blkxIgnore, blkxFree:
		// Zero-fill. blkxIgnore (0x00000002) was missing, and hdiutil emits it
		// in every UDZO it writes, so reading ANY Apple-produced compressed
		// image failed with "unsupported block type 0x00000002". Nothing in
		// the package's own output uses it, which is why no test saw it.
	case blkxRaw:
		if _, err := f.ReadAt(out, int64(dataOffset+r.compressedOffset)); err != nil {
			return nil, fmt.Errorf("blkx raw read: %w", err)
		}
	case blkxZlib:
		comp := make([]byte, r.compressedLength)
		if _, err := f.ReadAt(comp, int64(dataOffset+r.compressedOffset)); err != nil {
			return nil, fmt.Errorf("blkx zlib read: %w", err)
		}
		zr, err := zlib.NewReader(bytes.NewReader(comp))
		if err != nil {
			return nil, fmt.Errorf("blkx zlib open: %w", err)
		}
		defer zr.Close()
		if _, err := io.ReadFull(zr, out); err != nil {
			return nil, fmt.Errorf("blkx zlib decompress: %w", err)
		}
	case blkxLzfse:
		comp := make([]byte, r.compressedLength)
		if _, err := f.ReadAt(comp, int64(dataOffset+r.compressedOffset)); err != nil {
			return nil, fmt.Errorf("blkx lzfse read: %w", err)
		}
		dec, err := lzfse.Decompress(comp)
		if err != nil {
			return nil, fmt.Errorf("blkx lzfse decompress: %w", err)
		}
		if len(dec) != len(out) {
			return nil, fmt.Errorf("blkx lzfse: decompressed %d bytes, want %d", len(dec), len(out))
		}
		copy(out, dec)
	case blkxTerm:
		// terminator, no data
	default:
		return nil, fmt.Errorf("blkx: unsupported block type 0x%08x", r.blockType)
	}
	return out, nil
}

// ─── run builders ─────────────────────────────────────────────────────────────

func isZeroSector(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// buildRunsForRaw stores all sectors in a single RAW run plus a terminator.
func buildRunsForRaw(sectors []byte) ([]blkxRun, []byte) {
	n := uint64(len(sectors) / udifSectorSize)
	runs := []blkxRun{
		{blockType: blkxRaw, sectorNumber: 0, sectorCount: n,
			compressedOffset: 0, compressedLength: uint64(len(sectors))},
		{blockType: blkxTerm, sectorNumber: n},
	}
	return runs, append([]byte(nil), sectors...)
}

// buildRunsForZlib compresses sectors as a sequence of ZLIB runs
// (one run per udifZlibChunkSectors sectors).
const udifZlibChunkSectors = 2048 // 1 MiB per zlib run

func buildRunsForZlib(sectors []byte) ([]blkxRun, []byte) {
	n := len(sectors) / udifSectorSize
	var runs []blkxRun
	var dataFork []byte
	for i := 0; i < n; {
		j := i + udifZlibChunkSectors
		if j > n {
			j = n
		}
		count := uint64(j - i)
		chunk := sectors[i*udifSectorSize : j*udifSectorSize]
		var buf bytes.Buffer
		w, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
		w.Write(chunk)
		w.Close()
		compressed := buf.Bytes()
		off := uint64(len(dataFork))
		runs = append(runs, blkxRun{
			blockType: blkxZlib, sectorNumber: uint64(i), sectorCount: count,
			compressedOffset: off, compressedLength: uint64(len(compressed)),
		})
		dataFork = append(dataFork, compressed...)
		i = j
	}
	runs = append(runs, blkxRun{blockType: blkxTerm, sectorNumber: uint64(n)})
	return runs, dataFork
}

// buildRunsForSparse splits sectors into RAW (non-zero) and NOCOPY (zero) runs.
func buildRunsForSparse(sectors []byte) ([]blkxRun, []byte) {
	n := len(sectors) / udifSectorSize
	var runs []blkxRun
	var dataFork []byte
	for i := 0; i < n; {
		zero := isZeroSector(sectors[i*udifSectorSize : (i+1)*udifSectorSize])
		j := i + 1
		for j < n && isZeroSector(sectors[j*udifSectorSize:(j+1)*udifSectorSize]) == zero {
			j++
		}
		count := uint64(j - i)
		if zero {
			runs = append(runs, blkxRun{blockType: blkxNocopy, sectorNumber: uint64(i), sectorCount: count,
				compressedOffset: uint64(len(dataFork)), compressedLength: 0})
		} else {
			off := uint64(len(dataFork))
			chunk := sectors[i*udifSectorSize : j*udifSectorSize]
			dataFork = append(dataFork, chunk...)
			runs = append(runs, blkxRun{blockType: blkxRaw, sectorNumber: uint64(i), sectorCount: count,
				compressedOffset: off, compressedLength: uint64(len(chunk))})
		}
		i = j
	}
	runs = append(runs, blkxRun{blockType: blkxTerm, sectorNumber: uint64(n)})
	return runs, dataFork
}

// ─── plist parsing ────────────────────────────────────────────────────────────

type blkxPlistItem struct {
	Attributes string
	Data       []byte
	ID         string
	Name       string
}

// parsePlistBlkx extracts blkx table binaries from a UDIF XML plist.
func parsePlistBlkx(xmlData []byte) ([]blkxPlistItem, error) {
	// Strip DOCTYPE to avoid Go's strict XML parser rejecting it.
	clean := stripDoctype(xmlData)
	dec := xml.NewDecoder(bytes.NewReader(clean))
	if err := seekToBlkxArray(dec); err != nil {
		return nil, err
	}
	return parseBlkxArray(dec)
}

func stripDoctype(b []byte) []byte {
	s := string(b)
	if idx := strings.Index(s, "<!DOCTYPE"); idx >= 0 {
		if end := strings.Index(s[idx:], ">"); end >= 0 {
			s = s[:idx] + s[idx+end+1:]
		}
	}
	return []byte(s)
}

// seekToBlkxArray advances dec to just inside the <array> following <key>blkx</key>.
func seekToBlkxArray(dec *xml.Decoder) error {
	var lastKey string
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("udif: blkx array not found: %w", err)
		}
		t, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if t.Name.Local == "key" {
			var k string
			if err := dec.DecodeElement(&k, &t); err != nil {
				return err
			}
			lastKey = k
		} else if t.Name.Local == "array" && lastKey == "blkx" {
			return nil
		}
	}
}

// parseBlkxArray parses all <dict> entries inside the blkx <array>.
// Called after the <array> start element has been consumed.
func parseBlkxArray(dec *xml.Decoder) ([]blkxPlistItem, error) {
	var items []blkxPlistItem
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("udif: array parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "dict" {
				item, err := parseBlkxDictEntry(dec)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				return items, nil
			}
		}
	}
}

// parseBlkxDictEntry parses one <dict> entry inside the blkx array.
// Called after the <dict> start element has been consumed.
func parseBlkxDictEntry(dec *xml.Decoder) (blkxPlistItem, error) {
	var item blkxPlistItem
	var lastKey string
	for {
		tok, err := dec.Token()
		if err != nil {
			return item, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var k string
				if err := dec.DecodeElement(&k, &t); err != nil {
					return item, err
				}
				lastKey = k
			case "string":
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return item, err
				}
				switch lastKey {
				case "Attributes":
					item.Attributes = v
				case "ID":
					item.ID = v
				case "Name":
					item.Name = v
				}
			case "data":
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return item, err
				}
				// Strip ALL whitespace, not just newlines and spaces:
				// hdiutil indents its <data> with TABS, so a genuine Apple
				// image failed here at input byte 0 while every image this
				// package wrote itself decoded fine.
				clean := strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) {
						return -1
					}
					return r
				}, v)
				decoded, err := base64.StdEncoding.DecodeString(clean)
				if err != nil {
					return item, fmt.Errorf("udif: base64 decode blkx Data: %w", err)
				}
				item.Data = decoded
			default:
				if err := dec.Skip(); err != nil {
					return item, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				return item, nil
			}
		}
	}
}

// writePlistBlkx generates an Apple-style XML plist containing the blkx array.
func writePlistBlkx(items []blkxPlistItem) []byte {
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	sb.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	sb.WriteString("<plist version=\"1.0\">\n<dict>\n\t<key>resource-fork</key>\n\t<dict>\n\t\t<key>blkx</key>\n\t\t<array>\n")
	for _, item := range items {
		sb.WriteString("\t\t\t<dict>\n")
		sb.WriteString("\t\t\t\t<key>Attributes</key><string>" + item.Attributes + "</string>\n")
		sb.WriteString("\t\t\t\t<key>Data</key><data>" + base64.StdEncoding.EncodeToString(item.Data) + "</data>\n")
		sb.WriteString("\t\t\t\t<key>ID</key><string>" + item.ID + "</string>\n")
		sb.WriteString("\t\t\t\t<key>Name</key><string>" + item.Name + "</string>\n")
		sb.WriteString("\t\t\t</dict>\n")
	}
	sb.WriteString("\t\t</array>\n\t</dict>\n</dict>\n</plist>\n")
	return []byte(sb.String())
}

// ─── UDIF image I/O ───────────────────────────────────────────────────────────

// readAllUDIFSectors reads all sectors from a UDIF image into a flat byte slice.
// Returns the sector data and the koly block parsed from the image.
func readAllUDIFSectors(path string) ([]byte, kolyBlock, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, kolyBlock{}, err
	}
	defer f.Close()
	info, err := osStatFile(f)
	if err != nil {
		return nil, kolyBlock{}, err
	}
	if info.Size() < kolyBlockSize {
		return nil, kolyBlock{}, fmt.Errorf("udif: file too small for koly block")
	}
	buf := make([]byte, kolyBlockSize)
	if _, err := osReadAtFile(f, buf, info.Size()-kolyBlockSize); err != nil {
		return nil, kolyBlock{}, fmt.Errorf("udif: read koly: %w", err)
	}
	koly, err := parseKoly(buf)
	if err != nil {
		return nil, kolyBlock{}, err
	}
	// Multi-segment images (segmentCount > 1) are not supported.
	if koly.segmentCount > 1 {
		return nil, kolyBlock{}, fmt.Errorf("udif: multi-segment images are not supported (segmentCount=%d)", koly.segmentCount)
	}
	plistData := make([]byte, koly.xmlLength)
	if _, err := f.ReadAt(plistData, int64(koly.xmlOffset)); err != nil {
		return nil, kolyBlock{}, fmt.Errorf("udif: read plist: %w", err)
	}
	items, err := parsePlistBlkx(plistData)
	if err != nil {
		return nil, kolyBlock{}, err
	}
	sectors := make([]byte, koly.sectorCount*udifSectorSize)
	for _, item := range items {
		tbl, runs, err := parseBlkxTable(item.Data)
		if err != nil {
			return nil, kolyBlock{}, fmt.Errorf("udif: parse blkx: %w", err)
		}
		if err := fillSectorsFromRuns(f, tbl, runs, sectors); err != nil {
			return nil, kolyBlock{}, err
		}
		// Integrity is checked only on images THIS package wrote, and the
		// discriminator is honest rather than clever: our writer leaves the
		// koly checksum slots empty (type 0), hdiutil always fills them.
		//
		// The reason for the restriction is measured. Apple's CRC-32 over the
		// same sectors is not crc32.ChecksumIEEE of the sector bytes -- a
		// genuine hdiutil UDZO declares 0x53eff58f where we compute
		// 0xe823f5b4 -- so enforcing our definition on a foreign image would
		// condemn a perfectly good DMG as corrupt. Until Apple's variant is
		// worked out, the only checksums this package is entitled to judge
		// are its own.
		if koly.masterChecksum == 0 && koly.dataForkChecksum == 0 &&
			tbl.checksumType == blkxChecksumCRC32 && tbl.checksum != 0 {
			lo := tbl.sectorNumber * udifSectorSize
			hi := lo + tbl.sectorCount*udifSectorSize
			if hi > uint64(len(sectors)) {
				hi = uint64(len(sectors))
			}
			if got := crc32.ChecksumIEEE(sectors[lo:hi]); got != tbl.checksum {
				return nil, kolyBlock{}, fmt.Errorf("udif: blkx checksum mismatch (want 0x%08x, got 0x%08x)", tbl.checksum, got)
			}
		}
	}
	// The koly checksum slots are never verified: same reason as above, and
	// this package no longer writes them at all.
	return sectors, koly, nil
}

// fillSectorsFromRuns copies decompressed run data into the flat sectors buffer.
func fillSectorsFromRuns(f io.ReaderAt, tbl blkxTable, runs []blkxRun, sectors []byte) error {
	for _, r := range runs {
		if r.blockType == blkxTerm {
			break
		}
		data, err := decompressRun(f, tbl.dataOffset, r)
		if err != nil {
			return err
		}
		start := int64((tbl.sectorNumber + r.sectorNumber) * udifSectorSize)
		copy(sectors[start:], data)
	}
	return nil
}

// writeUDIF creates a UDIF image at path containing the provided sector data.
// variant selects the image type (use udifVariantCodes["UDRW"] etc.).
func writeUDIF(path string, sectors []byte, variant uint32) error {
	n := uint64(len(sectors) / udifSectorSize)
	var runs []blkxRun
	var dataFork []byte
	switch variant {
	case udifVariantCodes["UDSP"]:
		runs, dataFork = buildRunsForSparse(sectors)
	case udifVariantCodes["UDZO"]:
		runs, dataFork = buildRunsForZlib(sectors)
	default: // UDRW, UDRO, …
		runs, dataFork = buildRunsForRaw(sectors)
	}
	// Checksums.
	//
	// The blkx table carries a CRC-32 of the uncompressed sectors, which is
	// what a reader uses and what qemu-img checks. The two KOLY checksum
	// blocks are a different matter: declaring type 2 / 32 bits there makes
	// hdiutil VALIDATE them, and the value written here was rejected --
	// measured on macOS 26, one variable at a time:
	//
	//	imageVariant 2 + koly checksums type 2 -> "invalid checksum"
	//	imageVariant 2 + koly checksums type 0 -> attaches, mounts, reads
	//
	// So the koly blocks are left as type 0 ("no checksum"), which is a
	// legitimate UDIF encoding, rather than carrying a value that is wrong.
	// Writing a CORRECT koly checksum is worth doing; claiming one we cannot
	// compute is not, and an image that will not mount is a worse outcome
	// than one that is merely unverified.
	sectorsCRC := crc32.ChecksumIEEE(sectors)
	tblBytes := writeBlkxTable(blkxTable{sectorNumber: 0, sectorCount: n, dataOffset: 0}, runs, sectorsCRC)
	plistBytes := writePlistBlkx([]blkxPlistItem{
		{Attributes: "0x0050", Data: tblBytes, ID: "0", Name: "whole disk (UDIF)"},
	})
	xmlOff := uint64(len(dataFork))
	koly := kolyBlock{
		flags: 1, runningDataForkOffset: xmlOff, dataForkOffset: 0,
		dataForkLength: xmlOff, segmentNumber: 1, segmentCount: 1,
		xmlOffset: xmlOff, xmlLength: uint64(len(plistBytes)),
		imageVariant: imageVariantWholeDisk, sectorCount: n,
		// left zero on purpose: see the note above
		dataForkChecksum: 0,
		masterChecksum:   0,
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("udif write create: %w", err)
	}
	defer f.Close()
	for _, chunk := range [][]byte{dataFork, plistBytes} {
		if _, err := osWriteFile(f, chunk); err != nil {
			return fmt.Errorf("udif write: %w", err)
		}
	}
	kb := kolyToBytes(koly)
	if _, err := osWriteFile(f, kb[:]); err != nil {
		return fmt.Errorf("udif write koly: %w", err)
	}
	return nil
}

// ─── public API ───────────────────────────────────────────────────────────────

// DetectUDIFFormat returns the UDIF format string for the image at path
// (e.g. "UDRW", "UDSP") by parsing the koly trailer in pure Go.
func DetectUDIFFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("DetectUDIFFormat: %w", err)
	}
	defer f.Close()
	info, err := osStatFile(f)
	if err != nil {
		return "", fmt.Errorf("DetectUDIFFormat: stat: %w", err)
	}
	if info.Size() < kolyBlockSize {
		return "", fmt.Errorf("DetectUDIFFormat: file too small to be UDIF")
	}
	buf := make([]byte, kolyBlockSize)
	if _, err := osReadAtFile(f, buf, info.Size()-kolyBlockSize); err != nil {
		return "", fmt.Errorf("DetectUDIFFormat: read koly: %w", err)
	}
	koly, err := parseKoly(buf)
	if err != nil {
		return "", fmt.Errorf("DetectUDIFFormat: %w", err)
	}
	// The format is NOT in the koly block. imageVariant is a layout
	// discriminator (see the constants above); reading it as a format name
	// returned "UDRW" for a genuine Apple UDZO, which is how this went
	// unnoticed. Infer the format from the chunk types actually present.
	xml := make([]byte, koly.xmlLength)
	if _, err := osReadAtFile(f, xml, int64(koly.xmlOffset)); err != nil {
		return "", fmt.Errorf("DetectUDIFFormat: read plist: %w", err)
	}
	items, err := parsePlistBlkx(xml)
	if err != nil {
		return "", fmt.Errorf("DetectUDIFFormat: %w", err)
	}
	seen := map[uint32]bool{}
	for _, it := range items {
		_, runs, err := parseBlkxTable(it.Data)
		if err != nil {
			return "", fmt.Errorf("DetectUDIFFormat: %w", err)
		}
		for _, r := range runs {
			seen[r.blockType] = true
		}
	}
	switch {
	case seen[blkxZlib]:
		return "UDZO", nil
	case seen[blkxLzfse]:
		return "ULFO", nil
	case seen[blkxNocopy]:
		// A zero-fill run means zero sectors were ELIDED rather than stored,
		// which is the whole of what UDSP is. buildRunsForRaw never emits one
		// (it writes a single raw run covering every sector), so a NOCOPY run
		// distinguishes the two for images this package wrote.
		//
		// It is a weaker signal for foreign images: UDSP is a layout choice,
		// not a chunk encoding, so a sparse image whose sectors happen to be
		// all non-zero is genuinely indistinguishable from UDRW. The format is
		// simply not recorded in a UDIF file; anything here is inference.
		return "UDSP", nil
	case koly.imageVariant == imageVariantPartitioned:
		return "UDRO", nil
	default:
		// "UDRW" here means an uncompressed UDIF image whose sectors THIS
		// PACKAGE can rewrite in place, which is what dmg_block_device asks
		// for. It is NOT what hdiutil means by UDRW, and the difference is
		// not cosmetic:
		//
		//	hdiutil create -format UDRW  -> the raw volume, NO koly trailer,
		//	                                a file exactly the volume's size,
		//	                                "Format Description: raw read/write"
		//	an image written here        -> koly + blkx; hdiutil imageinfo
		//	                                calls it UDRO and macOS mounts it
		//	                                READ-ONLY, whatever imageVariant says
		//
		// So an image this package calls UDRW cannot be given to a user as a
		// writable disk image. See the note on WrapRaw.
		return "UDRW", nil
	}
}

// ConvertUDIF reads all sectors from src and writes them to dst in dstFormat.
// Supported write formats: "UDRW", "UDRO", "UDSP", "UDZO".
// "UDCO" (ADC) and "UDBZ" (bzip2) are not yet supported for writing.
// For UDSP, zero sectors are stored as NOCOPY (no data) runs.
// For UDZO, sectors are compressed with zlib.
func ConvertUDIF(src, dst, dstFormat string) error {
	code, ok := udifVariantCodes[dstFormat]
	if !ok {
		return fmt.Errorf("ConvertUDIF: unknown format %q", dstFormat)
	}
	switch dstFormat {
	case "UDCO", "UDBZ":
		return fmt.Errorf("ConvertUDIF: compression format %q is not yet supported for writing", dstFormat)
	}
	sectors, _, err := readAllUDIFSectors(src)
	if err != nil {
		return fmt.Errorf("ConvertUDIF read src: %w", err)
	}
	if err := writeUDIF(dst, sectors, code); err != nil {
		return fmt.Errorf("ConvertUDIF write dst: %w", err)
	}
	return nil
}

// IsUDIF returns true if path contains an Apple UDIF image (valid koly trailer).
func IsUDIF(path string) bool {
	_, err := DetectUDIFFormat(path)
	return err == nil
}

// UnpackToTemp extracts all sectors from the UDIF image at path into a
// temporary raw file. The caller is responsible for removing the temp file.
func UnpackToTemp(path string) (string, error) {
	sectors, _, err := readAllUDIFSectors(path)
	if err != nil {
		return "", fmt.Errorf("udif: UnpackToTemp: %w", err)
	}
	tmp, err := os.CreateTemp("", "udif-unpack-*.img")
	if err != nil {
		return "", fmt.Errorf("udif: UnpackToTemp: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := osWriteFile(tmp, sectors); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("udif: UnpackToTemp: write: %w", err)
	}
	if err := osCloseFile(tmp); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("udif: UnpackToTemp: close: %w", err)
	}
	return tmpPath, nil
}

// PackFromTemp creates a UDIF image at destPath containing the raw sectors
// from tmpPath, atomically replacing destPath. Like WrapRaw, what it writes
// is read-only to macOS however DetectUDIFFormat names it.
func PackFromTemp(tmpPath, destPath string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("udif: PackFromTemp: open: %w", err)
	}
	defer f.Close()
	info, err := osStatFile(f)
	if err != nil {
		return fmt.Errorf("udif: PackFromTemp: stat: %w", err)
	}
	rawSize := info.Size()
	sectorCount := (rawSize + udifSectorSize - 1) / udifSectorSize
	sectors := make([]byte, sectorCount*udifSectorSize)
	if _, err := osReadFullFile(f, sectors[:rawSize]); err != nil {
		return fmt.Errorf("udif: PackFromTemp: read: %w", err)
	}
	f.Close()
	tmp2, err := os.CreateTemp(filepath.Dir(destPath), "udif-pack-*.tmp")
	if err != nil {
		return fmt.Errorf("udif: PackFromTemp: create temp: %w", err)
	}
	tmpName := tmp2.Name()
	tmp2.Close()
	if err := writeUDIF(tmpName, sectors, udifVariantCodes["UDRW"]); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("udif: PackFromTemp: write udif: %w", err)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("udif: PackFromTemp: rename: %w", err)
	}
	return nil
}

// WrapRaw wraps the raw file at path in a UDIF container, in place.
//
// macOS mounts the result READ-ONLY and hdiutil imageinfo calls it UDRO: a
// UDIF container is read-only whatever its imageVariant says, and a writable
// image is the raw file with no container at all. DetectUDIFFormat reports
// "UDRW" for what this writes because the sectors can be rewritten in place
// by this package -- see the note there. Do not ship the result as a
// read/write image.
func WrapRaw(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("udif: WrapRaw: %w", err)
	}
	defer f.Close()
	info, err := osStatFile(f)
	if err != nil {
		return fmt.Errorf("udif: WrapRaw: stat: %w", err)
	}
	rawSize := info.Size()
	sectorCount := (rawSize + udifSectorSize - 1) / udifSectorSize
	sectors := make([]byte, sectorCount*udifSectorSize)
	if _, err := osReadFullFile(f, sectors[:rawSize]); err != nil {
		return fmt.Errorf("udif: WrapRaw: read: %w", err)
	}
	f.Close()
	return writeUDIF(path, sectors, udifVariantCodes["UDRW"])
}

// ResizeUDRW grows the UDIF image at path to newSizeBytes (rounded up to sector
// boundary). Shrinking is not supported. The image variant is preserved.
func ResizeUDRW(path string, newSizeBytes int64) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("ResizeUDRW: size must be positive, got %d", newSizeBytes)
	}
	sectors, koly, err := readAllUDIFSectors(path)
	if err != nil {
		return fmt.Errorf("ResizeUDRW read: %w", err)
	}
	newSectorCount := (newSizeBytes + udifSectorSize - 1) / udifSectorSize
	current := int64(len(sectors)) / udifSectorSize
	if newSectorCount < current {
		return fmt.Errorf("ResizeUDRW: shrink not supported (current %d sectors, requested %d)", current, newSectorCount)
	}
	if newSectorCount == current {
		return nil
	}
	grown := make([]byte, newSectorCount*udifSectorSize)
	copy(grown, sectors)
	return writeUDIF(path, grown, koly.imageVariant)
}
