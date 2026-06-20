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

| Run type   | Code         | Description                            |
|------------|--------------|----------------------------------------|
| RAW        | `0x00000001` | Uncompressed sectors stored verbatim   |
| NOCOPY     | `0x00000000` | Zero-filled sectors (no stored data)   |
| FREE       | `0x7FFFFFFE` | Unallocated, treated as zeros          |
| LZFSE      | `0x80000004` | LZFSE compressed (Apple, macOS 10.12+) |
| ZLIB       | `0x80000005` | zlib-compressed sectors                |
| Terminator | `0xFFFFFFFF` | End-of-table marker                    |

**Image variants:**

| Name   | Code | Read | Write | Description                           |
|--------|------|------|-------|---------------------------------------|
| `UDRW` | 1    | ✓    | ✓     | Read-write, fixed size                |
| `UDRO` | 2    | ✓    | ✓     | Read-only (stored as RAW)             |
| `UDCO` | 3    | ✓    | —     | ADC compressed (write not supported)  |
| `UDZO` | 4    | ✓    | ✓     | zlib compressed (1 MiB chunks)        |
| `UDBZ` | 5    | ✓    | —     | bzip2 compressed (write not supported)|
| `UDSP` | 11   | ✓    | ✓     | Sparse (NOCOPY runs for zero sectors) |

## Public API

```go
// DetectUDIFFormat returns "UDRW", "UDSP", etc. by reading the koly trailer.
func DetectUDIFFormat(path string) (string, error)

// IsUDIF returns true if path is a valid Apple UDIF image.
func IsUDIF(path string) bool

// ConvertUDIF reads all sectors from src and writes them to dst in dstFormat.
// Supported write formats: UDRW, UDRO, UDSP (sparse), UDZO (zlib-compressed).
// UDCO and UDBZ are not yet supported for writing and return an error.
// Checksums (CRC-32) are written in the output koly and blkx headers.
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
