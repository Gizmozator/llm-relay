package version

import "testing"

func TestState(t *testing.T) {
	for _, tc := range []struct {
		mine, theirs, want string
	}{
		// Plain tags order by the triple.
		{"v0.2.0", "v0.1.0", Older},
		{"v0.1.0", "v0.2.0", Newer},
		{"v0.1.0", "v0.1.0", Same},
		{"v1.0.0", "v0.99.99", Older},
		{"v0.1.10", "v0.1.9", Older}, // numeric, not lexical
		// Describe suffix: commits past the tag make it newer.
		{"v0.1.0-5-gabc1234", "v0.1.0", Older},
		{"v0.1.0", "v0.1.0-5-gabc1234", Newer},
		{"v0.1.0-5-gabc1234", "v0.1.0-3-gdef5678", Older},
		{"v0.1.0-5-gabc1234-dirty", "v0.1.0-5-gabc1234", Same}, // dirty is noise
		// No leading v is fine (git describe of an un-prefixed tag).
		{"0.2.0", "v0.1.0", Older},
		// Unparseable on either side: never flagged.
		{"dev", "v0.1.0", Unknown},
		{"v0.1.0", "dev", Unknown},
		{"abc1234", "def5678", Unknown}, // bare shas (untagged builds)
		{"", "v0.1.0", Unknown},
		{"v0.1", "v0.1.0", Unknown}, // incomplete triple
	} {
		if got := State(tc.mine, tc.theirs); got != tc.want {
			t.Errorf("State(%q,%q) = %s, want %s", tc.mine, tc.theirs, got, tc.want)
		}
	}
}
