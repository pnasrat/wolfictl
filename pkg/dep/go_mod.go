package dep

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb"
)

type GoStrategy struct {
	Strategy
}

// Probe checks for a go.mod file and returns true if one is found,
// otherwise false.
func (s GoStrategy) Probe(path string) bool {
	targetFile := filepath.Join(path, s.LockFileName())

	_, err := os.Stat(targetFile)
	if err != nil {
		return false
	}

	return true
}

// LockFileName returns the name of the Go lockfile, "go.mod".
func (s GoStrategy) LockFileName() string {
	return "go.mod"
}

// LockFileName returns the name of the local Go lockfile, "go.mod.local".
func (s GoStrategy) LocalLockFileName() string {
	return "go.mod.local"
}

// ChecksumFileName returns the name of the Go checksum file, "go.sum".
func (s GoStrategy) ChecksumFileName() string {
	return "go.sum"
}

// LocalChecksumFileName returns the name of the local Go checksum file, "go.sum.local".
func (s GoStrategy) LocalChecksumFileName() string {
	return "go.sum.local"
}

func loadModFile(path string) (*modfile.File, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return modfile.Parse(filepath.Base(path), content, nil)
}

// Rebase performs a rebase of the go.mod file.
// We do not process requires entries as it may point to a fork of the module
// which may not be compatible.
func (s GoStrategy) Rebase(upstreamFile, downstreamFile, outputFile string) error {
	upstreamModFile, err := loadModFile(upstreamFile)
	if err != nil {
		return fmt.Errorf("loading upstream go.mod file: %w", err)
	}

	downstreamModFile, err := loadModFile(downstreamFile)
	if err != nil {
		return fmt.Errorf("loading downstream go.mod file: %w", err)
	}

	newModFile := &modfile.File{Syntax: &modfile.FileSyntax{}}
	newModFile.AddGoStmt(upstreamModFile.Go.Version)
	newModFile.AddModuleStmt(upstreamModFile.Module.Mod.Path)

	if upstreamModFile.Toolchain != nil && upstreamModFile.Toolchain.Name != "" {
		newModFile.AddToolchainStmt(upstreamModFile.Toolchain.Name)
	}

	for _, downstreamPkg := range downstreamModFile.Require {
		for _, upstreamPkg := range upstreamModFile.Require {
			if upstreamPkg.Mod.Path == downstreamPkg.Mod.Path {
				var targetVersion string

				if semver.Compare(downstreamPkg.Mod.Version, upstreamPkg.Mod.Version) > 0 {
					targetVersion = downstreamPkg.Mod.Version
				} else {
					targetVersion = upstreamPkg.Mod.Version
				}

				newModFile.AddNewRequire(upstreamPkg.Mod.Path, targetVersion, upstreamPkg.Indirect)
			}
		}
	}

	newModFile.SetRequireSeparateIndirect(newModFile.Require)

	for _, upstreamPkg := range upstreamModFile.Exclude {
		newModFile.AddExclude(upstreamPkg.Mod.Path, upstreamPkg.Mod.Version)
	}

	for _, upstreamPkg := range upstreamModFile.Replace {
		newModFile.AddReplace(upstreamPkg.Old.Path, upstreamPkg.Old.Version, upstreamPkg.New.Path, upstreamPkg.New.Version)
	}

	for _, upstreamPkg := range upstreamModFile.Retract {
		newModFile.AddRetract(modfile.VersionInterval{Low: upstreamPkg.Low, High: upstreamPkg.High}, upstreamPkg.Rationale)
	}

	newModFile.Cleanup()

	payload, err := newModFile.Format()
	if err != nil {
		return fmt.Errorf("formatting rebased go.mod file: %w", err)
	}

	if err := os.WriteFile(outputFile, payload, 0o644); err != nil {
		return fmt.Errorf("writing rebased go.mod file: %w", err)
	}

	return nil
}

// UpdateChecksums creates a checksum file given an input lockfile.  This is usually the
// local lockfile.
func (s GoStrategy) UpdateChecksums(lockFile, outputFile string) error {
	originModFile, err := loadModFile(lockFile)
	if err != nil {
		return fmt.Errorf("while loading the lockfile: %w", err)
	}

	outFile, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("while opening the output file: %w", err)
	}
	defer outFile.Close()

	sumdbClient := sumdb.NewClient(&clientOps{})
	for _, originPkg := range originModFile.Require {
		lines, err := sumdbClient.Lookup(originPkg.Mod.Path, originPkg.Mod.Version)
		if err != nil {
			return fmt.Errorf("looking up %s/%s: %w", originPkg.Mod.Path, originPkg.Mod.Version, err)
		}

		for _, line := range lines {
			fmt.Fprintln(outFile, line)
		}

		lines, err = sumdbClient.Lookup(originPkg.Mod.Path, originPkg.Mod.Version + "/go.mod")
		if err != nil {
			return fmt.Errorf("looking up %s/%s/go.mod: %w", originPkg.Mod.Path, originPkg.Mod.Version, err)
		}

		for _, line := range lines {
			fmt.Fprintln(outFile, line)
		}
	}

	return nil
}

// From https://github.com/mkmik/getsum/blob/v0.1.0/pkg/modfetch/sumdb.go:
// clientOps is a dummy implementation that doesn't preserve the cache and thus doesn't fully partecipate
// in the transparency log verification.
// See https://github.com/golang/go/blob/master/src/cmd/go/internal/modfetch/sumdb.go for a fuller implementation
type clientOps struct{}

func (*clientOps) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte("sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"), nil
	}
	if strings.HasSuffix(file, "/latest") {
		// Looking for cached latest tree head.
		// Empty result means empty tree.
		return []byte{}, nil
	}
	return nil, fmt.Errorf("unknown config %s", file)
}

func (*clientOps) WriteConfig(file string, old, new []byte) error {
	// Ignore writes.
	return nil
}

func (*clientOps) ReadCache(file string) ([]byte, error) {
	return nil, fmt.Errorf("no cache")
}

func (*clientOps) WriteCache(file string, data []byte) {
	// Ignore writes.
}

func (*clientOps) Log(msg string) {
	log.Print(msg)
}

func (*clientOps) SecurityError(msg string) {
	log.Fatal(msg)
}

func init() {
	http.DefaultClient.Timeout = 1 * time.Minute
}

func (*clientOps) ReadRemote(path string) ([]byte, error) {
	name := "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"
	if i := strings.Index(name, "+"); i >= 0 {
		name = name[:i]
	}
	target := "https://" + name + path
	/*
		if *url != "" {
			target = *url + path
		}
	*/
	resp, err := http.Get(target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %v: %v", target, resp.Status)
	}
	data, err := ioutil.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}
