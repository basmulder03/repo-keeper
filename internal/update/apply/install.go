// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

const maxBinary = 256 << 20 // generous bound for one extracted executable

// Binaries are the executables a release archive carries; the daemon is required, the tray is optional.
var Binaries = []string{"repo-keeper", "repo-keeper-tray"}

// PreviousSuffix is appended to a binary's path to keep what was there before an update, for rollback.
const PreviousSuffix = ".previous"

// extract returns the wanted executables from a release archive. Only exact top-level names are read; nothing is
// ever written under a name taken from the archive, and nothing is executed.
func extract(archive []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || (name != Binaries[0] && name != Binaries[1]) {
			continue
		}
		if h.Size > maxBinary {
			return nil, fmt.Errorf("%s is implausibly large (%d bytes)", name, h.Size)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	if _, ok := out[Binaries[0]]; !ok {
		return nil, errors.New("the archive does not contain repo-keeper")
	}
	for name, b := range out {
		if !bytes.HasPrefix(b, []byte("\x7fELF")) {
			return nil, fmt.Errorf("%s is not a Linux executable", name)
		}
	}
	return out, nil
}

// Result lists what an update replaced.
type Result struct {
	Replaced []string // binaries now at the new version
	Previous []string // the kept copies (rollback sources)
}

type step struct{ name, dst, tmp string }

// Install replaces the binary at target (and the tray next to it, if present) with the ones in archive. The old file
// is kept beside it with PreviousSuffix. The new file is fully written and synced under a temporary name and then
// renamed over the target, so the path never stops existing; nothing happens unless every file was extracted and
// written first.
func Install(archive []byte, target string) (Result, error) {
	files, err := extract(archive)
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Dir(target)
	steps := []step{{name: Binaries[0], dst: target}}
	tray := filepath.Join(dir, Binaries[1])
	if _, ok := files[Binaries[1]]; ok {
		if _, err := os.Stat(tray); err == nil {
			steps = append(steps, step{name: Binaries[1], dst: tray})
		}
	}
	for i := range steps {
		f, err := os.CreateTemp(dir, ".repo-keeper-update-*")
		if err != nil {
			discard(steps)
			return Result{}, fmt.Errorf("cannot write next to %s (installed by a package manager, or not yours to modify?): %w", target, err)
		}
		steps[i].tmp = f.Name()
		_, werr := f.Write(files[steps[i].name])
		serr := f.Sync()
		cerr := f.Close()
		// #nosec G302 -- an executable must be executable
		if err := errors.Join(werr, serr, cerr, os.Chmod(f.Name(), 0o755)); err != nil { //nolint:gosec // see #nosec above
			discard(steps)
			return Result{}, err
		}
	}
	var res Result
	for _, s := range steps {
		prev := s.dst + PreviousSuffix
		_ = os.Remove(prev)
		if _, err := os.Lstat(s.dst); err == nil {
			if err := os.Link(s.dst, prev); err != nil { // keep the old inode under the previous name
				discard(steps)
				return res, fmt.Errorf("keeping the previous %s: %w", s.name, err)
			}
			res.Previous = append(res.Previous, prev)
		}
		if err := os.Rename(s.tmp, s.dst); err != nil {
			discard(steps)
			return res, fmt.Errorf("replacing %s: %w", s.dst, err)
		}
		res.Replaced = append(res.Replaced, s.dst)
	}
	return res, nil
}

// discard removes temporary files that were not renamed into place.
func discard(steps []step) {
	for _, s := range steps {
		if s.tmp != "" {
			_ = os.Remove(s.tmp) // already renamed ones no longer exist under this name
		}
	}
}

// Rollback puts the kept previous binaries back. It is a swap, so rolling back twice returns to the newer version.
func Rollback(target string) (Result, error) {
	var res Result
	dir := filepath.Dir(target)
	dsts := []string{target}
	if tray := filepath.Join(dir, Binaries[1]); fileExists(tray + PreviousSuffix) {
		dsts = append(dsts, tray)
	}
	if !fileExists(target + PreviousSuffix) {
		return res, fmt.Errorf("there is no previous version kept next to %s (nothing was updated here)", target)
	}
	for _, dst := range dsts {
		prev, swap := dst+PreviousSuffix, dst+".swap"
		_ = os.Remove(swap)
		if err := os.Link(dst, swap); err != nil {
			return res, fmt.Errorf("preparing the rollback of %s: %w", dst, err)
		}
		if err := os.Rename(prev, dst); err != nil {
			_ = os.Remove(swap)
			return res, fmt.Errorf("restoring %s: %w", dst, err)
		}
		if err := os.Rename(swap, prev); err != nil {
			return res, fmt.Errorf("keeping the replaced %s: %w", dst, err)
		}
		res.Replaced = append(res.Replaced, dst)
		res.Previous = append(res.Previous, prev)
	}
	return res, nil
}

func fileExists(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}
