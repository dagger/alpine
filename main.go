// A Dagger Module to integrate with Alpine Linux

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	goapk "chainguard.dev/apko/pkg/apk/apk"
	"dagger.io/dagger/core"
	"github.com/dagger/alpine/internal/dagger"
	"golang.org/x/mod/semver"
)

type Distro string

const (
	DistroAlpine Distro = "ALPINE"
	DistroWolfi  Distro = "WOLFI"
)

func New(
	// Hardware architecture to build for
	// +optional
	arch string,
	// Alpine branch to download packages from
	// +optional
	// +default="edge"
	branch string,
	// APK packages to install
	// +optional
	packages []string,
	// Extra repositories to add to the package resolver
	// +optional
	extraRepositories []string,
	// Extra keys needed to authenticate the extra repositories
	// +optional
	extraKeyURLs []string,

	// Alpine distribution to use
	// +optional
	// +default="ALPINE"
	distro Distro,
) (Alpine, error) {
	if arch == "" {
		arch = runtime.GOARCH
	}
	goArch := arch
	if apkArch := goapk.ArchToAPK(arch); apkArch != arch {
		arch = apkArch
	}

	switch {
	case branch == "edge":
	case semver.IsValid("v" + branch):
		branch = "v" + branch
		fallthrough
	case semver.IsValid(branch):
		// discard anything after major.minor (that's how alpine branches are named)
		branch = semver.MajorMinor(branch)
	default:
		return Alpine{}, fmt.Errorf("invalid branch %s", branch)
	}

	return Alpine{
		Distro:            distro,
		Branch:            branch,
		Arch:              arch,
		Packages:          packages,
		ExtraRepositories: extraRepositories,
		ExtraKeyURLs:      extraKeyURLs,

		GoArch: goArch,
	}, nil
}

// An Alpine Linux configuration
type Alpine struct {
	// The distro to use
	Distro Distro
	// The hardware architecture to build for
	Arch string
	// The Alpine branch to download packages from
	Branch string
	// The APK packages to install
	Packages []string
	// Extra repositories to add to the package resolver
	ExtraRepositories []string
	// Where to download additional keys from
	ExtraKeyURLs []string

	// the GOARCH equivalent of Arch
	// +private
	GoArch string
}

// Build an Alpine Linux container
func (m *Alpine) Container(ctx context.Context) (*dagger.Container, error) {
	pkgs, err := m.resolve(ctx)
	if err != nil {
		return nil, err
	}

	ctr := core.NewContainer(dagger.ContainerOpts{Platform: dagger.Platform("linux/" + m.GoArch)})
	ctr, err = m.withPkgs(ctx, ctr, pkgs)
	if err != nil {
		return nil, fmt.Errorf("failed to create package container: %w", err)
	}

	// NOTE: "apk add" will not work in this container. Generally this is a good
	// thing since it's more efficient to install all packages here and keep
	// the output container immutable.
	//
	// However, if the need to support that arises the following would be needed:
	// * /etc/apk/arch with the architecture written
	// * /etc/apk/repositories with the list of repo urls
	// * /etc/apk/world with each top level installed package name written
	// * /etc/apk/keys/ with a file for each key
	// * /lib/apk/db/installed with package metadata for each installed pkg
	//   * the goapk.PackageToInstalled + AddInstalledPackage code helps here

	ctr = ctr.WithEnvVariable("PATH", strings.Join([]string{
		"/usr/local/sbin",
		"/usr/local/bin",
		"/usr/sbin",
		"/usr/bin",
		"/sbin",
		"/bin",
	}, ":"))

	return ctr, nil
}

// Resolve the packages Container would install, with all their dependencies,
// to the exact versions the repositories offer now. Each entry is
// "name=version", so passing the result back as packages installs this same
// set for as long as the repositories keep these versions.
// +cache="session"
func (m *Alpine) Resolve(ctx context.Context) ([]string, error) {
	pkgs, err := m.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return packagePins(pkgs), nil
}

// packagePins returns each package as "name=version".
func packagePins(pkgs []*goapk.RepositoryPackage) []string {
	pins := make([]string, len(pkgs))
	for i, pkg := range pkgs {
		pins[i] = pkg.Name + "=" + pkg.Version
	}
	return pins
}

// resolve reads the repositories' current indexes and resolves the packages,
// with all their dependencies, in install order.
func (m *Alpine) resolve(ctx context.Context) ([]*goapk.RepositoryPackage, error) {
	var branch *goapk.ReleaseBranch
	var repos []string
	var basePkgs []string

	switch m.Distro {
	case DistroAlpine:
		releases, err := alpineReleases()
		if err != nil {
			return nil, fmt.Errorf("failed to get alpine releases: %w", err)
		}
		branch = releases.GetReleaseBranch(m.Branch)
		if branch == nil {
			return nil, fmt.Errorf("failed to get alpine release %q", m.Branch)
		}
		repos = alpineRepositories(*branch)

		basePkgs = []string{"alpine-baselayout", "alpine-release", "busybox", "apk-tools"}
	case DistroWolfi:
		releases := wolfiReleases()
		if m.Branch != "edge" {
			return nil, fmt.Errorf("failed to get wolfi release %q", m.Branch)
		}
		branch = releases.GetReleaseBranch("main")
		if branch == nil {
			return nil, fmt.Errorf("failed to get wolfi release %q", m.Branch)
		}
		repos = wolfiRepositories()

		basePkgs = []string{
			"busybox",
		}
	default:
		return nil, fmt.Errorf("unknown distro %q", m.Distro)
	}

	keys, err := fetchKeys(*branch, m.Arch, m.ExtraKeyURLs)
	if err != nil {
		return nil, fmt.Errorf("failed to get keys: %w", err)
	}
	if m.ExtraRepositories != nil {
		repos = append(repos, m.ExtraRepositories...)
	}
	indexes, err := goapk.GetRepositoryIndexes(ctx, repos, keys, m.Arch, goapk.WithHTTPClient(http.DefaultClient))
	if err != nil {
		return nil, fmt.Errorf("failed to get indexes: %w", err)
	}
	allPkgs := make([]string, 0, len(m.Packages)+len(basePkgs))
	allPkgs = append(allPkgs, basePkgs...)
	allPkgs = append(allPkgs, m.Packages...)
	return resolvePackages(ctx, indexes, allPkgs)
}

// resolvePackages resolves pkgs, with all their dependencies, against
// indexes, in install order.
func resolvePackages(ctx context.Context, indexes []goapk.NamedIndex, pkgs []string) ([]*goapk.RepositoryPackage, error) {
	pkgResolver := goapk.NewPkgResolver(ctx, indexes)
	repoPkgs, conflicts, err := pkgResolver.GetPackagesWithDependencies(ctx, pkgs, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get packages: %w", err)
	}
	if len(conflicts) > 0 {
		// conflicts aren't fatal, the most common one is "musl" on Wolfi, which only
		// happens because some packages there explicitly say it cannot be a dependency
		// (i.e. it's dependent on musl not being installed because it wants glibc).
		fmt.Printf("unfatal package conflicts: %v\n", conflicts)
	}
	return repoPkgs, nil
}

func (m *Alpine) withPkgs(
	ctx context.Context,
	ctr *dagger.Container,
	repoPkgs []*goapk.RepositoryPackage,
) (*dagger.Container, error) {
	setupBase := core.NewContainer().From("busybox")

	type apkPkg struct {
		name string
		dir  *dagger.Directory
	}

	// Nothing here reads a package, so nothing is unpacked until something
	// needs the container's files. Each package's unpack stays its own result,
	// shared by every image that installs that package.
	alpinePkgs := make([]*apkPkg, len(repoPkgs))
	for i, pkg := range repoPkgs {
		url := pkg.URL()
		mntPath := filepath.Join("/mnt", filepath.Base(url))
		outDir := "/out"

		unpacked := setupBase.
			WithMountedFile(mntPath, core.HTTP(url)).
			WithMountedDirectory(outDir, core.NewDirectory()).
			WithWorkdir(outDir).
			WithExec([]string{"tar", "-xf", mntPath, "--exclude=.*"})

		dir := unpacked.Directory(outDir)

		// HACK: /lib64 is a link, so don't overwrite it
		// - wolfi-baselayout links /lib64 -> /lib
		// - ld-linux installs to /lib64
		// TODO: this should *probably* apply to /usr/lib64/ and
		// /usr/local/lib64/ as well
		//
		// Checking whether the package has a lib64 would unpack it now, so the
		// move applies to every package: a step after the unpack makes sure
		// lib64 exists, which leaves the unpack the result every image shares.
		// A lib64 link is left alone, to be resolved inside the package as
		// before. A package with neither lib nor lib64 now carries an empty lib.
		if m.Distro == DistroWolfi && pkg.PackageName() != "wolfi-baselayout" {
			dir = unpacked.WithExec([]string{"sh", "-c", "[ -L lib64 ] || mkdir -p lib64"}).Directory(outDir)
			dir = dir.
				WithDirectory("/lib", dir.Directory("/lib64")).
				WithoutDirectory("/lib64")
		}

		alpinePkgs[i] = &apkPkg{
			name: pkg.PackageName(),
			dir:  dir,
		}
	}

	// Layering a package onto a container that has nothing left to run copies
	// it at once, so the packages up to busybox are stacked as a directory and
	// become the root filesystem in one lazy step. Later packages layer onto
	// the busybox exec, which is still to run.
	installBusyboxSymlinks := false
	rootfs := core.NewDirectory()
	for _, pkg := range alpinePkgs {
		if installBusyboxSymlinks {
			ctr = ctr.WithDirectory("/", pkg.dir)
			continue
		}
		rootfs = rootfs.WithDirectory("/", pkg.dir)

		if pkg.name == "busybox" || pkg.name == "busybox-full" {
			// We copy apko and don't run scripts, but with a special exception for busybox,
			// whose symlinks are usually installed by a script.
			installBusyboxSymlinks = true

			// Run now to install basic symlinks for things like `sh`, `ls`, etc.
			// This is load-bearing for the corner case of the engine having custom CA certs that
			// it will try to install in this container if/when the ca-certificates package is
			// installed.
			ctr = ctr.WithRootfs(rootfs).WithExec([]string{"/bin/busybox", "--install", "-s"})
		}
	}

	// run again at the end to ensure all symlinks are installed, including those
	// normally only created after /etc/busybox-paths.d/busybox is updated
	if installBusyboxSymlinks {
		ctr = ctr.WithExec([]string{"/bin/busybox", "--install", "-s"})
	} else {
		ctr = ctr.WithRootfs(rootfs)
	}

	return ctr, nil
}

func fetchKeys(branch goapk.ReleaseBranch, arch string, extraKeyURLs []string) (map[string][]byte, error) {
	urls := branch.KeysFor(arch, time.Now())
	if extraKeyURLs != nil {
		urls = append(urls, extraKeyURLs...)
	}
	keys := make(map[string][]byte)
	for _, u := range urls {
		res, err := http.Get(u)
		if err != nil {
			return nil, fmt.Errorf("failed to get alpine key at %s: %w", u, err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unable to get alpine key at %s: %v", u, res.Status)
		}
		keyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read alpine key at %s: %w", u, err)
		}

		// URL may have %40 instead of @, which confuses later goapk code that looks for
		// this key name
		u = strings.ReplaceAll(u, "%40", "@")

		keys[filepath.Base(u)] = keyBytes
	}
	return keys, nil
}
