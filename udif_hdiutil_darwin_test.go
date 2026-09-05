// The judge this package was missing.
//
// Its only external reader was qemu-img, which is lenient: every image it
// wrote passed there while macOS refused all of them. Two defects hid behind
// that for as long as the package existed — imageVariant written as a format
// code, and a koly checksum hdiutil validates and rejects. A test suite whose
// only outside opinion comes from a forgiving reader is a test suite that
// cannot find this class of bug.
//
// Gated on darwin AND on hdiutil being present, so CI elsewhere still passes.

//go:build darwin

package dmg

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var devRe = regexp.MustCompile(`/dev/disk\d+`)

// TestHdiutilAttachesWhatWeWrite writes a UDIF the way this package does and
// asks macOS itself to mount it.
func TestHdiutilAttachesWhatWeWrite(t *testing.T) {
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skip("hdiutil not present")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.dmg")

	// An HFS+ volume made by hdiutil itself, so the PAYLOAD is beyond doubt
	// and the only thing under test is the container this package writes.
	src := filepath.Join(dir, "src.dmg")
	mk := exec.Command(hdiutil, "create", "-size", "8m", "-fs", "HFS+",
		"-volname", "UDIFProbe", "-layout", "NONE", "-ov", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Skipf("hdiutil create unavailable here: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WrapRaw(path); err != nil {
		t.Fatalf("WrapRaw: %v", err)
	}

	out, err := exec.Command(hdiutil, "attach", "-nobrowse", "-readonly", path).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil refused an image this package wrote: %v\n%s", err, out)
	}
	dev := devRe.FindString(string(out))
	if dev == "" {
		t.Fatalf("attached but no device in output:\n%s", out)
	}
	// Detach ONLY the device this test attached. A broad match here once
	// ejected another session's volumes on this machine.
	t.Cleanup(func() {
		if o, err := exec.Command(hdiutil, "detach", dev).CombinedOutput(); err != nil {
			t.Errorf("detach %s: %v\n%s", dev, err, o)
		}
	})
	if !strings.Contains(string(out), "/Volumes/") {
		t.Errorf("attached but did not mount:\n%s", out)
	}
}
