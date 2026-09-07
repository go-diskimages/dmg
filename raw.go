package dmg

import (
	"fmt"
	"os"
)

// A raw image is a disk image with no container at all: the sectors ARE the
// file. It is what `hdiutil create -format UDRW` writes -- hdiutil imageinfo
// reports "raw read/write" for one -- and the only shape macOS will mount
// read/write. Everything else in this package is about the UDIF container,
// which the system mounts read-only however its trailer is stamped.

// osReadWholeFile is a seam, like the ones in udif.go: it is what makes the
// unreadable-file branch reachable without a test that depends on not being
// root.
var osReadWholeFile = os.ReadFile

// classify opens path and reports whether it is a UDIF image, handing back
// its koly trailer when it is. A raw image gets a zero koly and no error.
//
// Every reader below goes through here so the "could not open / stat / read
// the trailer" cases are answered once, in one place, rather than four times
// with four slightly different messages.
func classify(path string) (*os.File, kolyBlock, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, kolyBlock{}, false, err
	}
	info, err := osStatFile(f)
	if err != nil {
		f.Close()
		return nil, kolyBlock{}, false, err
	}
	if info.Size() < kolyBlockSize {
		return f, kolyBlock{}, false, nil
	}
	buf := make([]byte, kolyBlockSize)
	if _, err := osReadAtFile(f, buf, info.Size()-kolyBlockSize); err != nil {
		f.Close()
		return nil, kolyBlock{}, false, err
	}
	koly, err := parseKoly(buf)
	if err != nil {
		// Not a koly trailer, so the file is raw -- which is a shape, not a
		// failure.
		return f, kolyBlock{}, false, nil
	}
	return f, koly, true, nil
}

// isUDIFImage reports whether path ends in a koly block, which is what makes
// a file a UDIF image rather than a raw one.
func isUDIFImage(path string) (bool, error) {
	f, _, udif, err := classify(path)
	if err != nil {
		return false, err
	}
	f.Close()
	return udif, nil
}

// readSectors returns every sector of the image at path, whether it is a UDIF
// image or a raw one. A raw image has to be a whole number of sectors; a file
// that is neither is refused rather than padded, because padding a file that
// is not an image produces an image.
func readSectors(path string) ([]byte, error) {
	udif, err := isUDIFImage(path)
	if err != nil {
		return nil, err
	}
	if udif {
		sectors, _, err := readAllUDIFSectors(path)
		return sectors, err
	}
	b, err := osReadWholeFile(path)
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
	f, koly, udif, err := classify(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if !udif {
		info, err := osStatFile(f)
		if err != nil {
			return false, err
		}
		return info.Size() > 0 && info.Size()%udifSectorSize == 0, nil
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
	udif, err := isUDIFImage(path)
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
