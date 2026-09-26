// Copyright (c) 2026, go-diskimages
// SPDX-License-Identifier: BSD-3-Clause

package dmg

import (
	"embed"
	"fmt"
	"strings"
	"testing"
)

// ⛔ This package's own sources, so the rule below is a CONTROL and not a note.
//
//go:embed *.go
var ownSources embed.FS

// forbiddenCommands are the ways a test in this package could reach a block
// device. Each is a substring, matched case-insensitively, with what to do instead.
var forbiddenCommands = []struct{ fragment, instead string }{
	{"hdiutil\", \"attach", "ask hdiutil verify, imageinfo or convert instead: they read the file"},
	{"\"attach\"", "ask hdiutil verify, imageinfo or convert instead: they read the file"},
	{"\"detach\"", "there is nothing to detach if nothing was attached"},
	{"hdiutil\", \"create", "hdiutil create -fs attaches a device to format one; build the payload in Go, or use bytes that are not a filesystem at all"},
	{"diskutil", "a disk utility operates on devices; this package is about a file"},
	{"/dev/disk", "a device node has no business in a test about a file format"},
	{"mount_hfs", "mounting is a device operation"},
	{"newfs_", "formatting is a device operation"},
}

// TestNothingHereTouchesABlockDevice.
//
// A disk image is a FILE. Everything this package does to one can be done, and is
// done, by reading and writing that file -- and the judge that says whether macOS
// accepts what we write does it by reading too.
//
// This test exists because the judge did not always. It used to call
// `hdiutil attach` on every image it wrote, and `hdiutil create -fs HFS+` to make
// a mountable payload, which attaches a device of its own: two block devices per
// test run, on a workstation, to answer a question about a file format. One of
// those cleanups once ejected another session's volumes on this machine.
//
// The replacement is not weaker -- verify, imageinfo and convert between them
// refuse every defect the mounting test caught, measured one defect at a time --
// so there is no reason left to reach for a device, and this makes reaching for one
// fail here rather than in somebody's Finder.
func TestNothingHereTouchesABlockDevice(t *testing.T) {
	entries, err := ownSources.ReadDir(".")
	if err != nil {
		t.Fatalf("read own sources: %v", err)
	}
	// Premise: the embed actually picked the sources up. An empty list would make
	// every assertion below vacuously true, which is the failure mode of a sweep
	// that cannot read.
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			files = append(files, e.Name())
		}
	}
	if len(files) < 5 {
		t.Fatalf("embedded %d .go files, which cannot be right: this sweep would "+
			"pass by finding nothing", len(files))
	}
	var tests int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			tests++
		}
	}
	if tests == 0 {
		t.Fatal("no _test.go files were embedded, and they are what this checks")
	}
	t.Logf("swept %d files, %d of them tests", len(files), tests)

	for _, f := range files {
		b, err := ownSources.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := strings.ToLower(string(b))
		for _, bad := range forbiddenCommands {
			needle := strings.ToLower(bad.fragment)
			if !strings.Contains(body, needle) {
				continue
			}
			// This file names them all, in this table and in the comments above,
			// so it has to exempt itself -- and say that it does, because an
			// exemption nobody mentions is how a guard stops guarding.
			if f == "noblockdevices_test.go" {
				continue
			}
			t.Errorf("%s reaches for a block device (%q): %s", f, bad.fragment, bad.instead)
		}
	}
}

// TestTheGuardWouldActuallyFire is the ablation, kept in the suite rather than run
// once by hand: a sweep that cannot match is a sweep that reports zero.
func TestTheGuardWouldActuallyFire(t *testing.T) {
	sample := `out, err := exec.Command(hdiutil, "attach", "-nobrowse", path).CombinedOutput()`
	body := strings.ToLower(sample)
	var hit []string
	for _, bad := range forbiddenCommands {
		if strings.Contains(body, strings.ToLower(bad.fragment)) {
			hit = append(hit, bad.fragment)
		}
	}
	if len(hit) == 0 {
		t.Fatalf("the line this guard exists to catch matches none of its %d "+
			"patterns, so the sweep above proves nothing:\n  %s",
			len(forbiddenCommands), sample)
	}
	t.Log(fmt.Sprintf("the line an attach looks like matches: %v", hit))
}
