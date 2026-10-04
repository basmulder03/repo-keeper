// SPDX-License-Identifier: Apache-2.0

package update

import (
	"os"
	"path/filepath"
	"strings"
)

// Kind says who owns an installed binary and therefore who should update it.
type Kind string

// Install channels.
const (
	Tarball   Kind = "tarball"   // unpacked by the user (or install.sh): the only channel that may replace itself
	Nix       Kind = "nix"       // /nix/store
	Package   Kind = "package"   // .deb or .rpm under /usr
	Homebrew  Kind = "homebrew"  // Cellar
	Container Kind = "container" // image layer
)

// Channel is the detected install channel and, when the binary is not ours to replace, the owner's update command.
type Channel struct {
	Kind    Kind
	Exe     string
	Command string
}

// SelfUpdatable reports whether the explicit apply is allowed for this channel (ADR-0021).
func (c Channel) SelfUpdatable() bool { return c.Kind == Tarball }

// Probe is what channel detection may look at; tests supply their own.
type Probe struct {
	Exists func(path string) bool
}

// RealProbe looks at the real file system.
func RealProbe() Probe {
	return Probe{Exists: func(p string) bool { _, err := os.Stat(p); return err == nil }}
}

// Detect classifies the binary at exe. It only reads paths; it never runs a package manager.
func Detect(exe string, p Probe) Channel {
	if p.Exists == nil {
		p = RealProbe()
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	c := Channel{Exe: exe}
	switch {
	case strings.HasPrefix(exe, "/nix/store/"):
		c.Kind, c.Command = Nix, "nix profile upgrade repo-keeper   # or bump your flake input / Home Manager and rebuild"
	case p.Exists("/.dockerenv") || p.Exists("/run/.containerenv"):
		c.Kind, c.Command = Container, "pull the newer image and recreate the container"
	case strings.Contains(exe, "/Cellar/") || strings.Contains(exe, "/homebrew/"):
		c.Kind, c.Command = Homebrew, "brew upgrade repo-keeper"
	case strings.HasPrefix(exe, "/usr/") && !strings.HasPrefix(exe, "/usr/local/"):
		c.Kind = Package
		switch {
		case p.Exists("/etc/debian_version"):
			c.Command = "download the new .deb from the Install page, then: sudo apt install ./repo-keeper_<version>_linux_<arch>.deb"
		case p.Exists("/etc/fedora-release") || p.Exists("/etc/redhat-release"):
			c.Command = "download the new .rpm from the Install page, then: sudo dnf install ./repo-keeper_<version>_linux_<arch>.rpm"
		default:
			c.Command = "update it with the package manager that installed it"
		}
	default:
		c.Kind = Tarball
	}
	return c
}
