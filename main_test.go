package frrtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// envVersion overrides the version TestMain enforces, for the tripwire
// test's re-execution of this binary.
const envVersion = "FRRTEST_SELFTEST_VERSION"

func TestMain(m *testing.M) {
	// The module pins no FRR: the version this run enforces is whatever
	// the flake's FRR reports, so a flake update needs no Go change.
	o := testOracle()
	o.Version = os.Getenv(envVersion)
	if o.Version == "" {
		o.Version = zebraVersion(o)
	}

	o.Main(m)
}

// testOracle returns the oracle the module's own tests run.
func testOracle() Oracle {
	return Oracle{
		Daemons:   []string{"zebra", "isisd"},
		EnvPrefix: "FRRTEST",
		Host:      "frrtest-host",
		Peer:      "frrtest-frr",
	}
}

// zebraVersion returns the release o's zebra reports, or "" when there is
// no zebra to ask, which Main then reports as a missing FRR.
func zebraVersion(o Oracle) string {
	dir := os.Getenv(o.EnvPrefix + "_FRR")
	if dir == "" {
		dir = detect(o.Daemons)
	}

	out, err := exec.Command(filepath.Join(dir, "zebra"), "--version").Output()
	if err != nil {
		return ""
	}

	first, _, _ := strings.Cut(string(out), "\n")
	if f := strings.Fields(first); len(f) >= 3 {
		return f[2]
	}

	return ""
}
