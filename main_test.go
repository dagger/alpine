package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	goapk "chainguard.dev/apko/pkg/apk/apk"
)

// An index as a repository would serve it: git needs libcurl through a
// shared-library dependency, as Wolfi's git does. Each entry ends with a blank
// line, as in a real APKINDEX.
const baseIndex = `P:busybox
V:1.37.0-r0
A:aarch64
t:1700000000

P:libcurl
V:8.0.0-r0
A:aarch64
p:so:libcurl.so.4=4
t:1700000000

P:git
V:2.50.0-r0
A:aarch64
D:so:libcurl.so.4
t:1700000000

`

// The same repository after it published a newer build of a dependency.
const publishedIndex = baseIndex + `P:libcurl
V:8.1.0-r0
A:aarch64
p:so:libcurl.so.4=4
t:1800000000

`

func testIndexes(t *testing.T, index string) []goapk.NamedIndex {
	t.Helper()
	pkgs, err := goapk.ParsePackageIndex(strings.NewReader(index))
	if err != nil {
		t.Fatal(err)
	}
	repo := goapk.NewRepositoryFromComponents("https://packages.example", "", "os", "aarch64")
	// An unnamed index, as GetRepositoryIndexes returns for a repository that
	// isn't pinned: apko only installs a named index's packages when asked
	// for that name.
	return []goapk.NamedIndex{
		goapk.NewNamedRepositoryWithIndex("", repo.WithIndex(&goapk.APKIndex{Packages: pkgs})),
	}
}

func resolvePins(t *testing.T, index string, pkgs []string) []string {
	t.Helper()
	resolved, err := resolvePackages(context.Background(), testIndexes(t, index), pkgs)
	if err != nil {
		t.Fatal(err)
	}
	pins := packagePins(resolved)
	slices.Sort(pins)
	return pins
}

func TestResolvedPinsInstallTheSameSet(t *testing.T) {
	t.Parallel()

	pins := resolvePins(t, baseIndex, []string{"busybox", "git"})
	want := []string{"busybox=1.37.0-r0", "git=2.50.0-r0", "libcurl=8.0.0-r0"}
	if !slices.Equal(pins, want) {
		t.Fatalf("resolved %v, want %v", pins, want)
	}

	if got := resolvePins(t, baseIndex, pins); !slices.Equal(got, pins) {
		t.Errorf("resolving the pins gave %v, want %v", got, pins)
	}
}

func TestResolvedPinsHoldAfterAPublish(t *testing.T) {
	t.Parallel()

	pins := resolvePins(t, baseIndex, []string{"busybox", "git"})

	// Unpinned, the newer build replaces the old one.
	live := resolvePins(t, publishedIndex, []string{"busybox", "git"})
	if !slices.Contains(live, "libcurl=8.1.0-r0") {
		t.Fatalf("resolving by name after the publish gave %v, want libcurl=8.1.0-r0", live)
	}

	// Pinned, nothing changes.
	if got := resolvePins(t, publishedIndex, pins); !slices.Equal(got, pins) {
		t.Errorf("resolving the pins after the publish gave %v, want %v", got, pins)
	}
}
