// The judge this package was missing.
//
// Its only external reader was qemu-img, which is lenient: every image it wrote
// passed there while macOS refused all of them. Three defects hid behind that for
// as long as the package existed -- imageVariant written as a format code, a koly
// checksum hdiutil validates and rejects, and BuffersNeeded left at zero, which
// only a compressed image ever feels. A test suite whose only outside opinion
// comes from a forgiving reader is a test suite that cannot find this class of bug.
//
// ⛔ It used to ask that opinion by MOUNTING the image: `hdiutil attach`, plus
// `hdiutil create -fs HFS+` to make a mountable payload, which attaches a device
// of its own to format one. Two block devices per test, on a workstation, for a
// question about a file format.
//
// It asks three other ways now, none of which mounts anything:
//
//	hdiutil verify      checks both koly checksums and every blkx one
//	hdiutil imageinfo   parses the koly, the plist and the blkx tables
//	hdiutil convert     decodes every chunk through Apple's own decoder
//
// That is not a weaker judge, and it was measured rather than hoped for. Each of
// the three historical defects was put back in the writer and every judge asked:
//
//	imageVariant as a format code   verify REFUSED  imageinfo REFUSED  convert REFUSED
//	BuffersNeeded zero              verify REFUSED  imageinfo REFUSED  convert REFUSED  (UDZO only)
//	blkx header 200 bytes           verify REFUSED  imageinfo REFUSED  convert REFUSED
//	master checksum wrong by a bit  verify REFUSED  imageinfo OK       convert OK
//
// The last row is why verify is in the list and why it is not optional: it is the
// only one of the three that sees the koly's own checksums at all. And the second
// row is the one the mounting test existed for -- it survives, and still only on
// the compressed flavour, exactly as before.
//
// A data-fork checksum wrong by a bit never reaches hdiutil: this package's own
// reader refuses it first. That is a property worth having rather than a gap.
//
// Gated on darwin AND on hdiutil being present, so CI elsewhere still passes.

//go:build darwin

package dmg

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hdiutilPath finds hdiutil, or ends the test the way this lane promised.
func hdiutilPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("hdiutil")
	if err != nil {
		if requireHdiutil() {
			t.Fatalf("DMG_REQUIRE_HDIUTIL is set but hdiutil is not present: %v", err)
		}
		t.Skip("hdiutil not present")
	}
	return p
}

// sectorsForProbe is a payload for the container tests: 4 MiB of bytes that
// compress but are not uniform, so a UDZO of it has real runs.
//
// It is NOT a filesystem, and it does not need to be. Only mounting an image
// needed the bytes inside to be mountable; verify, imageinfo and convert read the
// container. Dropping that requirement is what removed the second block device.
func sectorsForProbe() []byte {
	b := make([]byte, 4<<20)
	for i := range b {
		b[i] = byte(i/udifSectorSize%7) | byte(i%3)
	}
	return b
}

// askHdiutil runs one verb over path and says whether Apple's tool accepted it.
func askHdiutil(t *testing.T, hdiutil string, args ...string) (string, bool) {
	t.Helper()
	out, err := exec.Command(hdiutil, args...).CombinedOutput()
	return string(out), err == nil
}

// TestHdiutilAcceptsWhatWeWrite asks macOS about a raw-backed UDIF and a UDZO,
// three ways each.
func TestHdiutilAcceptsWhatWeWrite(t *testing.T) {
	hdiutil := hdiutilPath(t)
	dir := t.TempDir()

	raw := filepath.Join(dir, "raw.dmg")
	if err := os.WriteFile(raw, sectorsForProbe(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WrapRaw(raw); err != nil {
		t.Fatalf("WrapRaw: %v", err)
	}
	zo := filepath.Join(dir, "zo.dmg")
	if err := ConvertUDIF(raw, zo, "UDZO"); err != nil {
		t.Fatalf("ConvertUDIF: %v", err)
	}

	for _, image := range []struct{ name, path string }{
		{"UDIF over a raw fork", raw},
		// The compressed path, which is what a real .dmg uses and which stayed
		// broken after the raw one was fixed: BuffersNeeded was left at zero, and
		// a raw run needs no decompression buffer, so only UDZO ever felt it.
		{"UDZO", zo},
	} {
		t.Run(image.name, func(t *testing.T) {
			out, ok := askHdiutil(t, hdiutil, "verify", image.path)
			if !ok {
				t.Errorf("hdiutil verify refused an image this package wrote:\n%s", out)
			}
			// Not just the exit status: "has no checksum" exits zero on some
			// releases, and an image nobody checked is not an image that checks
			// out. The word VALID is hdiutil's own verdict.
			if !strings.Contains(out, "is VALID") {
				t.Errorf("hdiutil verify did not call it VALID, so it may not have "+
					"checked anything:\n%s", out)
			}

			if out, ok := askHdiutil(t, hdiutil, "imageinfo", image.path); !ok {
				t.Errorf("hdiutil imageinfo refused it:\n%s", out)
			}

			// And Apple's own decoder over every chunk, compared byte for byte
			// with what went in. This is the assertion the mounting test could not
			// make: it proved the image opened, not that it held the right bytes.
			back := filepath.Join(t.TempDir(), "back.dmg")
			if out, ok := askHdiutil(t, hdiutil, "convert", image.path,
				"-format", "UDRW", "-o", back); !ok {
				t.Fatalf("hdiutil convert refused it:\n%s", out)
			}
			got, err := os.ReadFile(back)
			if err != nil {
				t.Fatal(err)
			}
			if want := sectorsForProbe(); !bytes.Equal(got, want) {
				t.Errorf("hdiutil decoded %d bytes and they are not what went in "+
					"(%d); first difference at %d", len(got), len(want),
					firstDifference(got, want))
			}
		})
	}
}

// TestHdiutilRefusesAKolyChecksumThatIsWrong is the control for the test above:
// if hdiutil verify accepted anything, "VALID" would mean nothing.
//
// The image is this package's own output with one bit of the master checksum
// flipped afterwards, so nothing else about it changes -- and that field is the
// one only verify looks at, which is why it is the field used here.
func TestHdiutilRefusesAKolyChecksumThatIsWrong(t *testing.T) {
	hdiutil := hdiutilPath(t)
	dir := t.TempDir()

	path := filepath.Join(dir, "good.dmg")
	if err := os.WriteFile(path, sectorsForProbe(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WrapRaw(path); err != nil {
		t.Fatalf("WrapRaw: %v", err)
	}
	// Premise: intact, hdiutil calls it VALID. Without this the refusal below
	// could be about anything.
	if out, ok := askHdiutil(t, hdiutil, "verify", path); !ok || !strings.Contains(out, "is VALID") {
		t.Fatalf("the intact image is not VALID, so this proves nothing:\n%s", out)
	}

	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// masterChecksum's value sits at koly+360, and the koly is the last 512 bytes.
	img[len(img)-kolyBlockSize+363] ^= 0x01
	bad := filepath.Join(dir, "bad.dmg")
	if err := os.WriteFile(bad, img, 0o600); err != nil {
		t.Fatal(err)
	}

	if out, ok := askHdiutil(t, hdiutil, "verify", bad); ok {
		t.Errorf("hdiutil verify accepted an image whose master checksum is wrong, "+
			"so calling the good one VALID says nothing:\n%s", out)
	}
}

// requireHdiutil reports whether this lane promised hdiutil would be there.
//
// DMG_REQUIRE_HDIUTIL=1 turns every exit in this file into a failure. The macOS
// lane sets it, because the whole reason this file exists is that the package's
// only other external reader -- qemu-img -- accepts images macOS refuses, and a
// judge that can quietly not run is worth no more than the lenient one it was
// added to correct.
func requireHdiutil() bool { return os.Getenv("DMG_REQUIRE_HDIUTIL") != "" }
