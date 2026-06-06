package dmg

import (
	"fmt"
	"io"
	"os"
)

// Format is the DMG disk image format.
// A Format value satisfies the diskimage_format.Format interface defined in
// github.com/go-diskimages/interface without requiring an import of that
// module (Go structural typing).
type Format struct{}

// Name returns "dmg".
func (Format) Name() string { return "dmg" }

// Create creates a new UDIF UDRW image at path with a blank raw payload of
// sizeBytes (rounded up to the nearest 512-byte sector boundary).
func (Format) Create(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("dmg: Create: size must be positive, got %d", sizeBytes)
	}
	sectorCount := (sizeBytes + udifSectorSize - 1) / udifSectorSize
	sectors := make([]byte, sectorCount*udifSectorSize)
	return writeUDIF(path, sectors, udifVariantCodes["UDRW"])
}

// Detect returns (true, nil) if path is an Apple UDIF image.
// Returns (false, nil) if the file exists but is not UDIF.
// Returns (false, err) if the file cannot be accessed.
func (Format) Detect(path string) (bool, error) {
	_, err := DetectUDIFFormat(path)
	if err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return false, statErr
		}
		return false, nil
	}
	return true, nil
}

// ToRaw extracts the raw sector payload from the UDIF image at src and writes
// it to dst. The progress writer w is currently unused.
func (Format) ToRaw(src, dst string, _ io.Writer) error {
	tmp, err := UnpackToTemp(src)
	if err != nil {
		return fmt.Errorf("dmg: ToRaw: %w", err)
	}
	if renameErr := os.Rename(tmp, dst); renameErr == nil {
		return nil
	}
	defer os.Remove(tmp)
	return dmgCopyFile(tmp, dst)
}

// Resize changes the virtual size of the UDIF image at path to newSizeBytes
// (rounded up to sector boundary). Both grow and shrink are supported.
// The image variant is preserved.
func (Format) Resize(path string, newSizeBytes int64) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("dmg: Resize: size must be positive, got %d", newSizeBytes)
	}
	sectors, koly, err := readAllUDIFSectors(path)
	if err != nil {
		return fmt.Errorf("dmg: Resize: %w", err)
	}
	newSectorCount := (newSizeBytes + udifSectorSize - 1) / udifSectorSize
	current := int64(koly.sectorCount)
	if newSectorCount == current {
		return nil
	}
	var newSectors []byte
	if newSectorCount > current {
		newSectors = make([]byte, newSectorCount*udifSectorSize)
		copy(newSectors, sectors)
	} else {
		newSectors = sectors[:newSectorCount*udifSectorSize]
	}
	return writeUDIF(path, newSectors, koly.imageVariant)
}

// dmgCopyFile copies src to dst as a plain byte copy.
func dmgCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("dmg: copy: open src: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("dmg: copy: open dst: %w", err)
	}
	defer out.Close()
	if _, err := ioCopyFiles(out, in); err != nil {
		return fmt.Errorf("dmg: copy: %w", err)
	}
	return nil
}
