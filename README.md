<p align="center"><img src="https://raw.githubusercontent.com/go-diskimages/brand/main/social/go-diskimages-dmg.png" alt="go-diskimages/dmg" width="720"></p>

# dmg

Pure-Go Apple UDIF disk image helpers. No external tools (`hdiutil`, `plutil`) required. Works on all platforms.

## Module

```
github.com/go-diskimages/dmg
```

## Apple UDIF format

The native Apple disk image format used by macOS. A UDIF file ends with a 512-byte `koly` block (big-endian) that contains:

- image variant code (UDRW, UDSP, …)
- data fork offset/length
- offset and length of an embedded XML plist

The plist carries an array of `blkx` (mish) tables, each describing a sequence of sector runs. Each run is one of:

| Run type   | Code         | Description                             |
|------------|--------------|-----------------------------------------|
| RAW        | `0x00000001` | Uncompressed sectors stored verbatim    |
| NOCOPY     | `0x00000000` | Zero-filled sectors (no stored data)    |
| IGNORE     | `0x00000002` | Zero-filled; stored length may be set   |
| FREE       | `0x7FFFFFFE` | Unallocated, treated as zeros           |
| ADC        | `0x80000004` | ADC compressed — the `UDCO` flavour, decoded by [`go-compressions/adc`](https://github.com/go-compressions/adc) |
| ZLIB       | `0x80000005` | zlib compressed — the `UDZO` flavour    |
| BZIP2      | `0x80000006` | bzip2 compressed — the `UDBZ` flavour   |
| LZFSE      | `0x80000007` | LZFSE compressed — the `ULFO` flavour   |
| Terminator | `0xFFFFFFFF` | End-of-table marker                     |

These four numbers were read off images `hdiutil` itself wrote — a raw file
converted to each flavour in turn, and the block types in the resulting `blkx`
tables read back. This table used to give `0x80000004` as LZFSE, which is ADC:
a real `UDCO` image was handed to the LZFSE decoder, and a real `ULFO` image
refused as an unknown type. Only `UDZO` was ever read correctly, because only
`UDZO`'s number happened to be right. Nothing saw it because every test for a
compressed flavour built its image out of these same constants, so a wrong one
agreed with itself.

LZMA (`0x80000008`) is a real block type this package does not decode. A run of
one is refused by number rather than guessed at.

**Image variants:**

| Name   | Code | Read | Write | Description                           |
|--------|------|------|-------|---------------------------------------|
| `UDRW` | 1    | ✓    | ✓     | Read-write, fixed size                |
| `UDRO` | 2    | ✓    | ✓     | Read-only (stored as RAW)             |
| `UDCO` | 3    | ✓    | —     | ADC compressed                        |
| `UDZO` | 4    | ✓    | ✓     | zlib compressed (1 MiB chunks)        |
| `UDBZ` | 5    | ✓    | —     | bzip2 compressed                      |
| `ULFO` | —    | ✓    | —     | LZFSE compressed                      |
| `UDSP` | 11   | ✓    | ✓     | Sparse (NOCOPY runs for zero sectors) |

A flavour marked read-only here is **refused by name** when something tries to
write it back, not re-encoded as something else. That refusal carries weight:
`UDRO` maps onto the raw writer, so a `UDBZ` misdetected as `UDRO` came back
uncompressed. While the reader rejected bzip2 that could never happen — the
read failed first — which is to say the broken decoder was the only thing
standing between a resize and a silently re-encoded image.

The four compressed flavours are tested against images `hdiutil` wrote, all
four made from the same raw file, each asserted to contain the block type its
flavour uses before its decode is believed.

## Integrity

Both CRC-32s a UDIF image carries are **verified on read**, on every image and
not only on ones this package wrote:

| | covers | catches |
|---|---|---|
| koly `dataForkChecksum` | the compressed bytes at `dataForkOffset`, `dataForkLength` of them | damage to the stored bytes, whatever the codec |
| blkx `UDIFChecksum` | **the bytes that table's producing runs put in the image** | sectors that came out wrong from bytes that were intact |

Both are plain `crc32.ChecksumIEEE`.

**A zero-fill run contributes nothing to the blkx checksum** — not its sectors and
not zeros standing in for them. The writer computes the value over what it
compresses, and a zero-fill chunk compresses nothing. Measured on a UDZO whose
table mixes the two: three zlib runs and two `IGNORE` runs over 8192 sectors,
declared `0xe614f115`, which is the CRC-32 of the 1 073 664 bytes the three zlib
runs produce and of nothing else — `0x86ff7e28` over all 8192 sectors.

That distinction is easy to miss and was missed here once. Nine `hdiutil` images
agreed with the simpler "all this table's sectors" rule, and **every one of them
turned out to contain no zero-fill run at all**: `hdiutil` emits one only when it
recognises the filesystem inside and can see its unused space, so converting a
raw file never produces one however large or however compressible. `IGNORE` is the
witnessed case; `NOCOPY` and `FREE` are treated the same way on the same argument,
with no image available here to confirm it, and the code says so.

The fork checksum is the one that matters most, because **ADC and LZFSE carry no
internal check at all**: without it a damaged run of either decodes to whatever
it decodes to. zlib's own Adler-32 does not save a zlib run either —
`io.ReadFull` stops at the output length and never reaches the trailer. Measured
before this was enforced: one flipped byte in a 5 KiB UDZO gave back a 900 KiB
image with a different SHA-256, the right length, and **no error**.

`masterChecksum` is documented as CRC-32 over every uncompressed sector byte and
is **not** enforced: every image available to measure against leaves its slot
empty, `hdiutil` included, so there is no witness for that one and this package
does not pretend to know it.

## Public API

```go
// DetectUDIFFormat returns "UDRW", "UDSP", etc. by reading the koly trailer.
func DetectUDIFFormat(path string) (string, error)

// IsUDIF returns true if path is a valid Apple UDIF image.
func IsUDIF(path string) bool

// ConvertUDIF reads all sectors from src and writes them to dst in dstFormat.
// Supported write formats: UDRW, UDRO, UDSP (sparse), UDZO (zlib-compressed).
// UDCO, UDBZ and ULFO can be read but not written, and are refused by name.
// Checksums (CRC-32) are written in the output blkx headers.
// Multi-segment images are not supported for reading and return an error.
func ConvertUDIF(src, dst, dstFormat string) error

// ResizeUDRW grows a UDIF image to newSizeBytes (rounded up to 512-byte boundary).
// The image variant is preserved. Shrinking is not supported; use Format.Resize instead.
func ResizeUDRW(path string, newSizeBytes int64) error

// UnpackToTemp extracts all UDIF sectors to a raw temp file; caller must os.Remove it.
func UnpackToTemp(path string) (tmpPath string, err error)

// PackFromTemp wraps the raw file at tmpPath as a new UDIF UDRW image at destPath (atomic).
func PackFromTemp(tmpPath, destPath string) error

// WrapRaw converts an existing raw file at path into a UDIF UDRW image in-place (atomic).
func WrapRaw(path string) error
```

## Format interface

`Format` satisfies `github.com/go-diskimages/interface` (structural typing, no import needed):

```go
type Format struct{}

func (Format) Name() string                                 // "dmg"
func (Format) Create(path string, sizeBytes int64) error    // creates blank UDIF UDRW image
func (Format) Detect(path string) (bool, error)             // detects Apple UDIF by koly magic
func (Format) ToRaw(src, dst string, w io.Writer) error     // extracts UDIF sectors to raw file
func (Format) Resize(path string, newSizeBytes int64) error // grow or shrink UDIF image
```

## Examples

### Detect and convert

```go
import "github.com/go-diskimages/dmg"

// Detect format of an existing image.
variant, err := dmg.DetectUDIFFormat("disk.dmg")
// variant == "UDRW"

// Convert to sparse (zero sectors stored without data).
err = dmg.ConvertUDIF("disk.dmg", "disk.sparse.dmg", "UDSP")

// Convert to zlib-compressed UDZO.
err = dmg.ConvertUDIF("disk.dmg", "disk.z.dmg", "UDZO")

// Grow to 2 GiB.
err = dmg.ResizeUDRW("disk.dmg", 2<<30)
```

### Wrap a raw file and unpack

```go
import "github.com/go-diskimages/dmg"

// Wrap a raw image as a UDIF UDRW container (atomic, in-place).
err := dmg.WrapRaw("disk.raw") // disk.raw is replaced by the UDIF image

// Extract payload for direct manipulation.
tmp, err := dmg.UnpackToTemp("disk.dmg")
defer os.Remove(tmp)

// Repack after modification.
err = dmg.PackFromTemp(tmp, "disk.dmg")
```
