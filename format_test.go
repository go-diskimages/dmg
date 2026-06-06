package dmg

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFormatName_DMG(t *testing.T) {
	if got := (Format{}).Name(); got != "dmg" {
		t.Fatalf("Name() = %q, want %q", got, "dmg")
	}
}

func TestFormatCreate_DMG_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	const size = 512 * 1024
	if err := (Format{}).Create(path, size); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ok, err := (Format{}).Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !ok {
		t.Fatal("created file is not detected as a DMG")
	}
}

func TestFormatCreate_DMG_InvalidSize(t *testing.T) {
	if err := (Format{}).Create(filepath.Join(t.TempDir(), "x.dmg"), 0); err == nil {
		t.Fatal("expected error for size=0")
	}
}

func TestFormatCreate_DMG_BadPath(t *testing.T) {
	if err := (Format{}).Create("/nonexistent/dir/disk.dmg", 1024); err == nil {
		t.Fatal("expected error for bad path")
	}
}

func TestFormatDetect_DMG_Valid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := (Format{}).Create(path, 512*1024); err != nil {
		t.Fatal(err)
	}
	ok, err := (Format{}).Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !ok {
		t.Fatal("Detect returned false for valid DMG")
	}
}

func TestFormatDetect_DMG_Invalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(path, make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := (Format{}).Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if ok {
		t.Fatal("Detect returned true for non-DMG file")
	}
}

func TestFormatDetect_DMG_NotExist(t *testing.T) {
	ok, err := (Format{}).Detect(filepath.Join(t.TempDir(), "nofile.dmg"))
	if ok {
		t.Fatal("Detect returned true for non-existent path")
	}
	if err == nil {
		t.Fatal("expected non-nil error for non-existent path")
	}
}

func TestFormatToRaw_DMG_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	dmgPath := filepath.Join(dir, "disk.dmg")
	rawPath := filepath.Join(dir, "disk.raw")
	const size = 512 * 1024
	if err := (Format{}).Create(dmgPath, size); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := (Format{}).ToRaw(dmgPath, rawPath, io.Discard); err != nil {
		t.Fatalf("ToRaw: %v", err)
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		t.Fatalf("stat raw: %v", err)
	}
	if info.Size() != size {
		t.Errorf("raw size = %d, want %d", info.Size(), size)
	}
}

func TestFormatToRaw_DMG_BadSrc(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst.raw")
	if err := (Format{}).ToRaw("/nonexistent.dmg", dst, io.Discard); err == nil {
		t.Fatal("expected error for non-existent src")
	}
}

func TestFormatResize_DMG_InvalidSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := (Format{}).Resize(path, 0); err == nil {
		t.Fatal("expected error for size=0")
	}
}

func TestFormatResize_DMG_BadSrc(t *testing.T) {
	if err := (Format{}).Resize("/nonexistent.dmg", 512); err == nil {
		t.Fatal("expected error for non-existent path")
	}
}

func TestFormatResize_DMG_Grow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := (Format{}).Create(path, 4096); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := (Format{}).Resize(path, 8192); err != nil {
		t.Fatalf("Resize grow: %v", err)
	}
	raw := filepath.Join(t.TempDir(), "out.raw")
	if err := (Format{}).ToRaw(path, raw, io.Discard); err != nil {
		t.Fatalf("ToRaw after grow: %v", err)
	}
	info, _ := os.Stat(raw)
	if info.Size() < 8192 {
		t.Errorf("payload size = %d, want >= 8192", info.Size())
	}
}

func TestFormatResize_DMG_Shrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.dmg")
	if err := (Format{}).Create(path, 8192); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := (Format{}).Resize(path, 4096); err != nil {
		t.Fatalf("Resize shrink: %v", err)
	}
	raw := filepath.Join(t.TempDir(), "out.raw")
	if err := (Format{}).ToRaw(path, raw, io.Discard); err != nil {
		t.Fatalf("ToRaw after shrink: %v", err)
	}
	info, _ := os.Stat(raw)
	if info.Size() != 4096 {
		t.Errorf("payload size = %d, want 4096", info.Size())
	}
}
