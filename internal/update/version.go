// SPDX-License-Identifier: Apache-2.0

// Package update finds out whether a newer release exists and where this binary came from. It never changes anything
// on disk except its own small state file; replacing a binary lives in the separate apply package, which only the
// explicit `update` command may import (ADR-0021).
package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a semantic version (https://semver.org); build metadata is ignored for ordering.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // pre-release identifiers, empty for a final release
}

var semver = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseVersion accepts "1.2.3", "v1.2.3-beta.4" and the like; anything else (a git describe, "dev") is an error.
func ParseVersion(s string) (Version, error) {
	m := semver.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("update: %q is not a release version", s)
	}
	v := Version{}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
	}
	return v, nil
}

// String formats the version without a leading "v".
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Prerelease reports whether this is a pre-release such as 0.1.0-beta.5.
func (v Version) Prerelease() bool { return len(v.Pre) > 0 }

// Compare orders versions per SemVer 2.0: -1, 0 or 1. A pre-release sorts before its release, numeric identifiers
// sort numerically and before alphanumeric ones, and a longer identifier list wins when the shared prefix is equal.
func (v Version) Compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if c := cmpInt(p[0], p[1]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := cmpIdent(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(v.Pre), len(o.Pre))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(an, bn)
	case aerr == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}
