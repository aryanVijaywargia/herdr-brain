package herdr

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

// minVersion is the oldest herdr that Brain supports.
const minVersion = "0.9.1"

// TestedVersions returns the herdr releases that passed the conformance test and the eval
// delivery and exclusion stages. Add a release to the list only after those pass.
func TestedVersions() []string {
	return []string{
		"0.9.1",
		"0.9.3", // #256, PR #297
	}
}

// tested reports whether herdr v is one of the TestedVersions.
func tested(v string) bool { return slices.Contains(TestedVersions(), v) }

var versionRe = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// CheckServer checks the version of the running herdr server. The server version
// decides, not the version of the herdr binary: a newer binary can talk to an older server
// that still runs. The hook does not call it, so that it stays inside its time cap.
func (c *Client) CheckServer(ctx context.Context) (version string, untested bool, err error) {
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return "", false, err
	}
	untested, err = checkVersion(v)
	if err != nil {
		return "", false, err
	}
	return v, untested, nil
}

// checkVersion refuses a herdr version older than 0.9.1 with the fix. For a supported
// version that is not in the tested list it returns untested = true: Brain keeps working
// and `brain status` warns.
func checkVersion(v string) (untested bool, err error) {
	got, gotPre, err := parseVersion(v)
	if err != nil {
		return false, err
	}
	floor, _, err := parseVersion(minVersion)
	if err != nil {
		panic(err) // minVersion is a constant
	}
	if c := slices.Compare(got, floor); c < 0 || c == 0 && gotPre {
		return false, fmt.Errorf("herdr %s is too old, Brain needs herdr %s or later: "+
			"run `herdr update`, then restart herdr so that the server runs the new version", v, minVersion)
	}
	return !tested(v), nil
}

// parseVersion reads MAJOR.MINOR.PATCH with an optional -prerelease and +build part. A
// prerelease comes before its release, as in semver.
func parseVersion(v string) (nums []int, pre bool, err error) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return nil, false, fmt.Errorf("herdr version %q is not MAJOR.MINOR.PATCH", v)
	}
	for _, s := range m[1:4] {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false, fmt.Errorf("herdr version %q: %w", v, err)
		}
		nums = append(nums, n)
	}
	return nums, m[4] != "", nil
}
