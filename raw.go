package dmg

import (
	"fmt"
	"os"
)

// A raw image is a disk image with no container at all: the sectors ARE the
// file. It is what `hdiutil create -format UDRW` writes -- hdiutil imageinfo
// reports "raw read/write" for one -- and the only shape macOS will mount
// read/write. Everything else in this package is about the UDIF container,
// which is read-only however its trailer is stamped.

// hasKolyTrailer reports whether path ends in a koly block, which is what
// makes a file a UDIF image rather than a raw one.
func hasKolyTrailer(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := osStatFile(f)
	if err != nil {
		return false, err
	}
	if info.Size() < kolyBlockSize {
		return false, nil
	}
	buf := make([]byte, kolyBlockSize)
	if _, err := osReadAtFile(f, buf, info.Size()-kolyBlockSize); err != nil {
		return false, err
	}
	_, err = parseKoly(buf)
	return err == nil, nil
}

// readSectors returns every sector of the image at path, whether it is a UDIF
// image or a raw one. A raw image has to be a whole number of sectors; a file
// that is neither is refused rather than padded, because padding a file that
// is not an image produces an image.
func readSectors(path string) ([]byte, error) {
	udif, err := hasKolyTrailer(path)
	if err != nil {
		return nil, err
	}
	if udif {
		sectors, _, err := readAllUDIFSectors(path)
		return sectors, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || len(b)%udifSectorSize != 0 {
		return nil, fmt.Errorf("udif: %s is neither a UDIF image nor a whole number of %d-byte sectors", path, udifSectorSize)
	}
	return b, nil
}

// InPlaceWritable reports whether the image's sectors sit in the file
// one-to-one, so a caller can map ReadAt and WriteAt straight onto it.
//
// That is true of a raw image, where the sectors are the whole file, and of a
// UDIF image whose runs are all stored uncompressed -- what this package
// writes for "UDRO", and what an in-place block device needs. It is false for
// a compressed or sparse image, where a sector's bytes are not at a fixed
// place in the file, or are not in it at all.
//
// It is NOT a claim about macOS: a UDIF image is mounted read-only by the
// system whatever this answers. It is a claim about the bytes.
func InPlaceWritable(path string) (bool, error) {
	udif, err := hasKolyTrailer(path)
	if err != nil {
		return false, err
	}
	if !udif {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		return info.Size() > 0 && info.Size()%udifSectorSize == 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := osStatFile(f)
	if err != nil {
		return false, err
	}
	buf := make([]byte, kolyBlockSize)
	if _, err := osReadAtFile(f, buf, info.Size()-kolyBlockSize); err != nil {
		return false, err
	}
	koly, err := parseKoly(buf)
	if err != nil {
		return false, err
	}
	xml := make([]byte, koly.xmlLength)
	if _, err := osReadAtFile(f, xml, int64(koly.xmlOffset)); err != nil {
		return false, err
	}
	items, err := parsePlistBlkx(xml)
	if err != nil {
		return false, err
	}
	for _, it := range items {
		_, runs, err := parseBlkxTable(it.Data)
		if err != nil {
			return false, err
		}
		for _, r := range runs {
			switch r.blockType {
			case blkxRaw, blkxTerm:
			default:
				return false, nil
			}
		}
	}
	return true, nil
}

// writeSectors puts sectors back at path in the shape they came in: a raw
// image stays raw, a UDIF image keeps the encoding its runs had.
//
// A format this package cannot write is refused rather than quietly rewritten
// as something else. That is not hypothetical: both resize paths used to pass
// the koly's imageVariant to a switch comparing it against FORMAT codes, so it
// never matched, and growing a UDZO image rewrote it uncompressed.
func writeSectors(path string, sectors []byte) error {
	udif, err := hasKolyTrailer(path)
	if err != nil {
		return err
	}
	if !udif {
		return os.WriteFile(path, sectors, 0o600)
	}
	format, err := DetectUDIFFormat(path)
	if err != nil {
		return err
	}
	enc, ok := containerFormats[format]
	if !ok {
		return fmt.Errorf("udif: cannot rewrite a %s image: this package has no writer for that encoding", format)
	}
	return writeUDIF(path, sectors, enc)
}
