// Package version parses and compares build versions. The stamp is
// `git describe --tags --always --dirty` (Makefile GIT_DESC): a semver tag
// (v0.1.0), optionally with the describe suffix for commits past the tag
// (v0.1.0-5-gabc1234) and a -dirty tail for a dirty tree. Untagged builds
// and dev builds are bare shas / "dev" — real strings, but with no
// ordering, so they are never flagged as old or new.
package version

import "regexp"

// describeRe matches the stamp shape above. The offset group (commits
// since the tag) only appears in git-describe form; -dirty may follow
// either form.
var describeRe = regexp.MustCompile(`^[vV]?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-fA-F]+)?(?:-dirty)?$`)

// States of a remote version relative to this build.
const (
	Older   = "older"
	Newer   = "newer"
	Same    = "same"
	Unknown = "unknown"
)

// State compares theirs against mine and describes THEIRS: Older means
// the other side runs less than this build, Newer that it runs more.
// Unparseable sides (dev builds, bare shas) yield Unknown — an unordered
// string must never raise or silence a warning by accident.
func State(mine, theirs string) string {
	a, okA := parse(mine)
	b, okB := parse(theirs)
	if !okA || !okB {
		return Unknown
	}
	switch {
	case a.before(b):
		return Newer
	case b.before(a):
		return Older
	default:
		return Same
	}
}

// Warn is one connected instance running a different version: Kind is
// spoke|hub|remote, State is older|newer relative to this build.
type Warn struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
	State   string `json:"state"`
}

type semver struct {
	major, minor, patch, offset int
}

// before is the describe-aware ordering: the tag triple first, then the
// commit offset (v0.1.0-5-g… is newer than plain v0.1.0).
func (a semver) before(b semver) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	return a.offset < b.offset
}

func parse(s string) (semver, bool) {
	m := describeRe.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false
	}
	v := semver{}
	atoi := func(g string) int {
		n := 0
		for _, c := range g {
			n = n*10 + int(c-'0')
		}
		return n
	}
	v.major, v.minor, v.patch = atoi(m[1]), atoi(m[2]), atoi(m[3])
	if m[4] != "" {
		v.offset = atoi(m[4])
	}
	return v, true
}
