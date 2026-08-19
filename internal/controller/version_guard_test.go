package controller

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
)

// TestOperatorVersionNotBehindLatestReleasedTag guards against exactly the
// class of bug OperatorVersion (see compat.go) hit for v0.4.0: a release
// gets tagged (and images built/pushed) but the separate release/version-
// bump step responsible for keeping that constant in sync gets skipped,
// leaving it silently pointing at an older release indefinitely.
//
// Tags are the source of truth here for the same reason
// scripts/bump-bundle-version.py's released_versions() also reads them
// instead of any in-repo file: they are the one thing that still reflects
// what was actually released after a fresh, tag-less shallow checkout.
// Fetching them the same non-fatal way that script does means this needs no
// CI workflow change — actions/checkout's default shallow clone omits tags.
func TestOperatorVersionNotBehindLatestReleasedTag(t *testing.T) {
	_ = exec.Command("git", "fetch", "--tags", "--quiet").Run()

	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		t.Skipf("could not list git tags, skipping version-drift check: %v", err)
	}

	var latest *semver.Version
	for _, tag := range strings.Fields(string(out)) {
		v, err := semver.NewVersion(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		if latest == nil || v.GreaterThan(latest) {
			latest = v
		}
	}
	if latest == nil {
		t.Skip("no vX.Y.Z tags found, skipping version-drift check")
	}

	have, err := semver.NewVersion(OperatorVersion)
	if err != nil {
		t.Fatalf("OperatorVersion %q is not valid semver", OperatorVersion)
	}

	if have.LessThan(latest) {
		t.Errorf(
			"OperatorVersion (%s) is behind the latest released tag (v%s) — "+
				"the release/version-bump step for that release didn't update "+
				"compat.go's OperatorVersion constant",
			OperatorVersion, latest,
		)
	}
}
