package dmg

// Regression tests for the UDIF/BLKX on-disk layout that guarantee
// interoperability with third-party UDIF readers (notably qemu-img's
// block/dmg.c). The historical bug: the mish (BLKXTable) header was emitted
// 200 bytes long with BlocksRunCount at +36, but the Apple/UDIF layout has a
// 204-byte header with BlocksRunCount at +200 and the first 40-byte chunk-run
// entry at +204. The 4-byte shift made every image qemu-img rejected with
// "length … for chunk 0 is larger than max".

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBlkxHeaderLayout asserts the exact byte offsets a conformant UDIF reader
// expects, independent of whether qemu-img is installed. This is the always-on
// guard against regressing the 204-byte mish header.
func TestBlkxHeaderLayout(t *testing.T) {
	if blkxHeaderSize != 204 {
		t.Fatalf("blkxHeaderSize = %d, want 204 (Apple/UDIF mish header size)", blkxHeaderSize)
	}
	runs := []blkxRun{
		{blockType: blkxRaw, sectorNumber: 0, sectorCount: 8, compressedOffset: 0, compressedLength: 8 * udifSectorSize},
		{blockType: blkxTerm, sectorNumber: 8},
	}
	const checksum = uint32(0xDEADBEEF)
	buf := writeBlkxTable(blkxTable{sectorNumber: 0, sectorCount: 8, dataOffset: 0}, runs, checksum)

	if got := binary.BigEndian.Uint32(buf[0:4]); got != blkxMagic {
		t.Fatalf("signature = %08x, want %08x", got, blkxMagic)
	}
	// BlocksRunCount must be at +200, NOT +36.
	if got := binary.BigEndian.Uint32(buf[200:204]); got != uint32(len(runs)) {
		t.Fatalf("BlocksRunCount@200 = %d, want %d", got, len(runs))
	}
	if leftover := binary.BigEndian.Uint32(buf[36:40]); leftover != 0 {
		t.Fatalf("BlockDescriptors@36 = %d, want 0 (run count must not live here)", leftover)
	}
	// UDIFChecksum at +64: type, size, first data word.
	if got := binary.BigEndian.Uint32(buf[64:68]); got != 0x00000002 {
		t.Fatalf("checksum.type@64 = %08x, want CRC-32 (2)", got)
	}
	if got := binary.BigEndian.Uint32(buf[68:72]); got != 0x00000020 {
		t.Fatalf("checksum.size@68 = %08x, want 0x20", got)
	}
	if got := binary.BigEndian.Uint32(buf[72:76]); got != checksum {
		t.Fatalf("checksum.data@72 = %08x, want %08x", got, checksum)
	}
	// First chunk-run entry must start at +204.
	off := 204
	if got := binary.BigEndian.Uint32(buf[off : off+4]); got != blkxRaw {
		t.Fatalf("chunk0.type@204 = %08x, want blkxRaw (%08x)", got, blkxRaw)
	}
	if got := binary.BigEndian.Uint64(buf[off+16 : off+24]); got != 8 {
		t.Fatalf("chunk0.sectorCount@220 = %d, want 8", got)
	}
	if got := binary.BigEndian.Uint64(buf[off+32 : off+40]); got != 8*udifSectorSize {
		t.Fatalf("chunk0.compressedLength@236 = %d, want %d", got, 8*udifSectorSize)
	}
	// The whole blob must be exactly header + N*40.
	if len(buf) != blkxHeaderSize+len(runs)*40 {
		t.Fatalf("mish blob len = %d, want %d", len(buf), blkxHeaderSize+len(runs)*40)
	}
}

// TestQemuImgInterop verifies that qemu-img (a strict third-party UDIF reader)
// can open every UDIF variant we emit and convert it byte-identically to raw.
// Gated on qemu-img availability so CI without qemu-img still passes.
func TestQemuImgInterop(t *testing.T) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not found in PATH; skipping interop check")
	}

	// Build a recognisable 4 MiB raw payload (non-zero then zero region so the
	// UDSP sparse path exercises both RAW and NOCOPY runs).
	const rawSize = 4 * 1024 * 1024
	raw := make([]byte, rawSize)
	for i := 0; i < rawSize/2; i++ {
		raw[i] = byte(i*7 + 3)
	}
	sectors := make([]byte, rawSize)
	copy(sectors, raw)

	for _, tc := range []struct {
		name string
		enc  runEncoding
	}{{"raw runs", encRaw}, {"zlib runs", encZlib}, {"elided zero runs", encSparse}} {
		variant := tc.name
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			dmgPath := filepath.Join(dir, "image.dmg")
			if err := writeUDIF(dmgPath, sectors, tc.enc); err != nil {
				t.Fatalf("writeUDIF(%s): %v", variant, err)
			}

			// qemu-img must be able to parse the trailer/blkx table.
			if out, err := exec.Command(qemuImg, "info", dmgPath).CombinedOutput(); err != nil {
				t.Fatalf("qemu-img info rejected our %s dmg: %v\n%s", variant, err, out)
			}

			// qemu-img convert must reproduce the original bytes exactly.
			outRaw := filepath.Join(dir, "out.raw")
			if out, err := exec.Command(qemuImg, "convert", "-O", "raw", dmgPath, outRaw).CombinedOutput(); err != nil {
				t.Fatalf("qemu-img convert of our %s dmg failed: %v\n%s", variant, err, out)
			}
			got, err := os.ReadFile(outRaw)
			if err != nil {
				t.Fatalf("read converted raw: %v", err)
			}
			if len(got) != len(sectors) {
				t.Fatalf("%s: converted raw is %d bytes, want %d", variant, len(got), len(sectors))
			}
			if !bytes.Equal(got, sectors) {
				t.Fatalf("%s: qemu-img convert produced non-identical bytes", variant)
			}

			// And it must still round-trip through our own reader.
			tmp, err := UnpackToTemp(dmgPath)
			if err != nil {
				t.Fatalf("%s: UnpackToTemp: %v", variant, err)
			}
			defer os.Remove(tmp)
			back, err := os.ReadFile(tmp)
			if err != nil {
				t.Fatalf("%s: read unpacked: %v", variant, err)
			}
			if !bytes.Equal(back, sectors) {
				t.Fatalf("%s: our reader round-trip mismatch", variant)
			}
		})
	}
}
