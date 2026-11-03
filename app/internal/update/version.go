package update

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is System.Version: up to four numbers, and a number that is not
// there is -1, which makes 1.0.11 smaller than 1.0.11.0, as in .NET.
type Version struct{ Major, Minor, Build, Revision int }

var versionRe = regexp.MustCompile(`\d+(?:\.\d+){0,3}`)

// NormalizeVersion is NormalizeVersion of UpdateService: a leading "v" is
// dropped and the first run of up to four dotted numbers is taken ("" when
// there is none), so "v1.0.11+abc" is "1.0.11".
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		v = v[1:]
	}
	return versionRe.FindString(strings.TrimSpace(v))
}

// ParseVersion is TryParseVersion: Version.TryParse for two to four numbers,
// and one number as major.0.
func ParseVersion(s string) (Version, bool) {
	parts := strings.Split(s, ".")
	if s == "" {
		return Version{}, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		if p == "" {
			return Version{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, false
		}
		nums[i] = n
	}
	v := Version{Build: -1, Revision: -1}
	switch len(nums) {
	case 1:
		v.Major, v.Minor = nums[0], 0
	case 2:
		v.Major, v.Minor = nums[0], nums[1]
	case 3:
		v.Major, v.Minor, v.Build = nums[0], nums[1], nums[2]
	case 4:
		v.Major, v.Minor, v.Build, v.Revision = nums[0], nums[1], nums[2], nums[3]
	default:
		return Version{}, false
	}
	return v, true
}

// Compare returns -1, 0 or 1.
func (a Version) Compare(b Version) int {
	for _, p := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Build, b.Build}, {a.Revision, b.Revision}} {
		switch {
		case p[0] < p[1]:
			return -1
		case p[0] > p[1]:
			return 1
		}
	}
	return 0
}
