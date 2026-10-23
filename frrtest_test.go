//go:build linux

package frrtest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestStart proves an instance runs the FRR the run enforces, under the
// name its logs are saved as, with the rendered configuration loaded
// and the harness network's addresses on its end of the pair.
func TestStart(t *testing.T) {
	t.Parallel()

	o := testOracle()
	i := startTest(t, o)

	if want := fmt.Sprintf("frrtest-TestStart-%d", os.Getpid()); i.Name() != want {
		t.Errorf("unexpected name: got %q, want %q", i.Name(), want)
	}

	version, err := i.Vtysh("show version")
	if err != nil {
		t.Fatalf("failed to run show version: %v: %s", err, version)
	}

	// The tripwire Main enforces against the daemon binary, asserted
	// again through the instance's own output so a mismatched vtysh
	// cannot hide it.
	if want := "FRRouting " + zebraVersion(o) + " "; !bytes.HasPrefix(version, []byte(want)) {
		t.Errorf("the instance must be %q, but reports: %s", want, version)
	}

	if got := runningConfig(t, i); !strings.Contains(got, "\n description rendered\n") {
		t.Errorf("the rendered configuration was not loaded:\n%s", got)
	}

	// zebra reads the addresses from the kernel, so they are there by the
	// time it answers.
	var ifs map[string]struct {
		IPAddresses []struct {
			Address string `json:"address"`
		} `json:"ipAddresses"`
	}

	if err := i.JSON(t, "show interface "+o.Peer+" json", &ifs); err != nil {
		t.Fatalf("failed to read interfaces: %v", err)
	}

	var got []string
	for _, a := range ifs[o.Peer].IPAddresses {
		got = append(got, a.Address)
	}

	var want []string
	for _, p := range i.Addresses().Peer {
		want = append(want, p.String())
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("unexpected %s addresses (-want +got):\n%s", o.Peer, diff)
	}
}

// TestInstanceConfigure proves configuration lines apply in nested mode
// as they would interactively, and that vtysh's complaint about a bad
// command reaches the caller.
func TestInstanceConfigure(t *testing.T) {
	t.Parallel()

	o := testOracle()
	i := startTest(t, o)

	i.Configure(t, "interface "+o.Peer, "description configured")
	if got := runningConfig(t, i); !strings.Contains(got, "\n description configured\n") {
		t.Errorf("the configured description was not applied:\n%s", got)
	}

	err := i.JSON(t, "show no such thing json", nil)
	if err == nil || !strings.Contains(err.Error(), "Unknown command") {
		t.Errorf("unexpected error for an unknown command: %v", err)
	}
}

// TestMainTripwire proves a run whose FRR is not the release it names
// fails before any test runs, and says why.
func TestMainTripwire(t *testing.T) {
	t.Parallel()

	// Stripped of the rig's markers, the binary starts over as the
	// process go test starts, which is where Main enforces the version.
	// Should the tripwire not fire, the run matches no tests and exits
	// zero.
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FRRTEST_NETNS_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}

	cmd.Env = append(cmd.Env, envVersion+"=0.0.0")

	out, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 {
		t.Fatalf("a mismatched FRR must fail the run with exit 1, but got %v: %s", err, out)
	}

	want := "frrtest: the oracle must be FRR 0.0.0, but isisd reports: isisd version " + zebraVersion(testOracle())
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("the failure lacks %q: %s", want, out)
	}
}

// TestOracleSetupAddresses proves an Oracle which sets one side of its
// Addresses and not the other fails Main before any namespace exists,
// rather than hosting an instance with no addresses on the other end.
func TestOracleSetupAddresses(t *testing.T) {
	t.Parallel()

	o := testOracle()
	o.Addresses.Host = DefaultAddresses().Host

	err := o.setup()
	if err == nil || !strings.Contains(err.Error(), "Oracle.Addresses") {
		t.Fatalf("unexpected error for one-sided addresses: %v", err)
	}
}

// TestStartLogDir proves every daemon's log and the init's are saved to
// $FRRTEST_LOGDIR under the instance's name when the instance ends.
//
// It cannot be parallel: it sets $FRRTEST_LOGDIR, which is process-wide.
func TestStartLogDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FRRTEST_LOGDIR", dir)

	// Registered before Start, this runs after the instance's teardown
	// has saved its logs.
	var name string
	t.Cleanup(func() {
		logs, err := os.ReadFile(filepath.Join(dir, name+".log"))
		if err != nil {
			t.Fatalf("failed to read saved logs: %v", err)
		}

		for _, want := range []string{"=== zebra.log ===\n", "=== isisd.log ===\n", "=== init.log ===\n"} {
			if !bytes.Contains(logs, []byte(want)) {
				t.Errorf("saved logs lack %q:\n%s", want, logs)
			}
		}
	})

	name = startTest(t, testOracle()).Name()
}

// A testConfig parameterizes testdata/frr.conf.tmpl.
type testConfig struct {
	// Interface is the instance's end of the veth pair and Description
	// the description the template gives it.
	Interface, Description string
}

// startTest starts an instance of o whose interface on the harness
// network carries the description "rendered".
func startTest(t *testing.T, o Oracle) *Instance {
	t.Helper()

	return o.Start(t, o.Render(t, filepath.Join("testdata", "frr.conf.tmpl"), testConfig{
		Interface:   o.Peer,
		Description: "rendered",
	}))
}

// runningConfig returns the instance's running configuration.
func runningConfig(t *testing.T, i *Instance) string {
	t.Helper()

	out, err := i.Vtysh("show running-config")
	if err != nil {
		t.Fatalf("failed to read running config: %v: %s", err, out)
	}

	return string(out)
}
