package frrtest

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/mdlayher/frrtest/netns"
)

const (
	// DefaultHostname is an instance's kernel hostname when
	// Oracle.Hostname is empty.
	DefaultHostname = "frr-interop"

	// DefaultTimeout bounds every poll when Oracle.Timeout is zero.
	DefaultTimeout = 60 * time.Second
)

// Addresses are the addresses on each end of the harness network: Host
// on the end in the test's namespace and Peer on the instance's.
type Addresses struct {
	Host, Peer []netip.Prefix
}

// DefaultAddresses returns the documentation prefixes: 192.0.2.1/24 and
// 2001:db8::1/64 on the host, 192.0.2.10/24 and 2001:db8::10/64 on the
// instance.
func DefaultAddresses() Addresses {
	return Addresses{
		Host: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.1/24"),
			netip.MustParsePrefix("2001:db8::1/64"),
		},
		Peer: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.10/24"),
			netip.MustParsePrefix("2001:db8::10/64"),
		},
	}
}

// An Oracle describes the FRR instances a suite hosts. A consumer
// declares one at package level, calls Main on it from TestMain and
// Start on it from tests, and never writes it: it is a value, and it
// holds no state.
type Oracle struct {
	// Version is the FRR release the daemons must report. Main fails the
	// run on a mismatch. The module pins nothing: each consumer's flake
	// supplies its FRR and names its version here.
	Version string

	// Daemons names the daemons under the FRR directory, zebra first,
	// which every consumer requires. Each is started as <dir>/<name> -u
	// root -g root, running as the namespace's root in place of the
	// missing frr user. Tripwire names the one whose --version enforces
	// Version; it defaults to the last daemon.
	Daemons  []string
	Tripwire string

	// EnvPrefix names the user-facing variables: <EnvPrefix>_FRR is the
	// daemon directory and <EnvPrefix>_LOGDIR is where logs are saved.
	// Lowercased, it also prefixes instance names and run directories.
	EnvPrefix string

	// Host and Peer name the veth pair: Host stays in the test's
	// namespace and Peer moves into the instance's.
	Host, Peer string

	// Addresses are the pair's addresses, DefaultAddresses when zero.
	// Setting one side without the other is an error Main reports,
	// since an end with no addresses is never what a suite means.
	Addresses Addresses

	// Hostname is the instance's kernel hostname, DefaultHostname when
	// empty.
	Hostname string

	// Timeout bounds every poll, DefaultTimeout when zero.
	Timeout time.Duration
}

// Main is the whole body of a consumer's TestMain. It never returns.
//
// Before any namespace exists, Main finds the daemons, checks each one
// and vtysh are present, and enforces Version. Any failure is fatal to
// the run, never a skip: a green run which tested nothing would be
// worse than a red one.
func (o Oracle) Main(m *testing.M) {
	netns.Main(m, netns.MainConfig{Setup: o.setup})
}

// setup validates o and its FRR installation, naming the daemon
// directory in the environment for every process the rig starts.
func (o Oracle) setup() error {
	switch {
	case len(o.Daemons) == 0:
		return errors.New("frrtest: Oracle.Daemons names no daemons")
	case o.EnvPrefix == "":
		return errors.New("frrtest: Oracle.EnvPrefix is empty")
	case o.Host == "", o.Peer == "":
		return errors.New("frrtest: Oracle.Host and Oracle.Peer must name the veth pair")
	case (o.Addresses.Host == nil) != (o.Addresses.Peer == nil):
		return errors.New("frrtest: Oracle.Addresses must set both Host and Peer, or neither for DefaultAddresses")
	}

	env := o.EnvPrefix + "_FRR"
	dir := os.Getenv(env)
	if dir == "" {
		dir = detect(o.Daemons)
		if dir == "" {
			return fmt.Errorf("frrtest: no FRR daemons found for the oracle; install FRR %s or set $%s",
				cmp.Or(o.Version, "with "+strings.Join(o.Daemons, " and ")), env)
		}

		if err := os.Setenv(env, dir); err != nil {
			return fmt.Errorf("frrtest: failed to set $%s: %w", env, err)
		}
	}

	for _, d := range o.Daemons {
		if _, err := os.Stat(filepath.Join(dir, d)); err != nil {
			return fmt.Errorf("frrtest: $%s does not provide %s: %w", env, d, err)
		}
	}

	if _, err := findVtysh(dir); err != nil {
		return fmt.Errorf("frrtest: %w", err)
	}

	// The version tripwire: oracle drift must be a loud failure, never a
	// quiet swap, since a changed oracle would be indistinguishable from
	// a regression in the code under test.
	if o.Version == "" {
		return errors.New("frrtest: Oracle.Version names no FRR release")
	}

	tripwire := cmp.Or(o.Tripwire, o.Daemons[len(o.Daemons)-1])
	out, err := exec.Command(filepath.Join(dir, tripwire), "--version").Output()
	if err != nil {
		return fmt.Errorf("frrtest: failed to run %s --version: %w", tripwire, err)
	}

	first, _, _ := strings.Cut(string(out), "\n")
	if f := strings.Fields(first); len(f) < 3 || f[2] != o.Version {
		return fmt.Errorf("frrtest: the oracle must be FRR %s, but %s reports: %s", o.Version, tripwire, first)
	}

	return nil
}

// Start starts an instance running the rendered configuration conf,
// torn down when tb ends. conf is the instance's whole frr.conf; the
// harness supplies vtysh.conf itself. It returns once every daemon
// accepts configuration through vtysh; protocol readiness is the
// caller's poll.
//
// Instances run one at a time, and a test may host only one: see
// netns.Start. When $<EnvPrefix>_LOGDIR is set, the instance's logs are
// saved there on teardown, and a failed test also logs them.
func (o Oracle) Start(tb testing.TB, conf []byte) *Instance {
	tb.Helper()

	if runtime.GOOS != "linux" {
		tb.Skip("frrtest: FRR instances require Linux")
	}

	dir := os.Getenv(o.EnvPrefix + "_FRR")
	vtysh, err := findVtysh(dir)
	if err != nil {
		tb.Fatalf("failed to find vtysh: %v", err)
	}

	// The vty sockets need a short directory: sun_path caps unix socket
	// paths at 108 bytes, which the nested directories of t.TempDir can
	// exceed.
	run, err := os.MkdirTemp("/tmp", sanitize(strings.ToLower(o.EnvPrefix)))
	if err != nil {
		tb.Fatalf("failed to create instance directory: %v", err)
	}

	tb.Cleanup(func() { _ = os.RemoveAll(run) })

	// The instance's config directory: the rendered config plus
	// vtysh.conf, whose presence keeps vtysh from polluting command
	// output with a missing-file complaint.
	etc := tb.TempDir()
	for file, b := range map[string][]byte{
		"frr.conf":   conf,
		"vtysh.conf": []byte("service integrated-vtysh-config\n"),
	} {
		if err := os.WriteFile(filepath.Join(etc, file), b, 0o644); err != nil {
			tb.Fatalf("failed to write %s: %v", file, err)
		}
	}

	// The group file bind-mounted over /etc/group: zebra's privs_init
	// insists on resolving the compiled-in frrvty group, which we alias
	// to gid 0, the only gid mapped in this user namespace.
	group, err := os.ReadFile("/etc/group")
	if err != nil {
		tb.Fatalf("failed to read /etc/group: %v", err)
	}

	if err := os.WriteFile(filepath.Join(run, "group"), append(group, "frrvty:x:0:\n"...), 0o644); err != nil {
		tb.Fatalf("failed to write group file: %v", err)
	}

	// No daemon runs as the missing frr user: the namespace's root plus
	// the group file sidestep the runas dance.
	daemons := make([]netns.Daemon, 0, len(o.Daemons))
	for _, d := range o.Daemons {
		daemons = append(daemons, netns.Daemon{
			Name: d,
			Path: filepath.Join(dir, d),
			Args: []string{
				"-u", "root",
				"-g", "root",
				"--vty_socket", run,
				"-i", "/run/frr/" + d + ".pid",
				"-z", "/run/frr/zserv.api",
			},
		})
	}

	addrs := o.addresses()
	name := o.instanceName(tb)
	ni := netns.Start(tb, netns.InstanceConfig{
		Dir:      run,
		Hostname: cmp.Or(o.Hostname, DefaultHostname),
		// Private tmpfs over /run and /var/lib makes FRR's compiled-in
		// state paths writable, and the group file satisfies zebra's
		// frrvty lookup.
		Mounts: []netns.Mount{
			{
				Type:   "tmpfs",
				Target: "/run",
			},
			{
				Type:   "tmpfs",
				Target: "/var/lib",
			},
			{
				Type:   "bind",
				Source: filepath.Join(run, "group"),
				Target: "/etc/group",
			},
		},
		Dirs: []string{"/run/frr", "/var/lib/frr"},
		Veth: netns.Veth{
			Host:      o.Host,
			Peer:      o.Peer,
			HostAddrs: addrs.Host,
			PeerAddrs: addrs.Peer,
		},
		Daemons: daemons,
	})

	// Registered after the rig's teardown, this runs before it: the logs
	// are read while the instance still runs.
	tb.Cleanup(func() { o.saveLogs(tb, name, ni.Logs()) })

	i := &Instance{
		o:     o,
		name:  name,
		vtysh: vtysh,
		run:   run,
		etc:   etc,
	}

	// The config load below needs every daemon's vty socket, which each
	// binds asynchronously after starting.
	i.Poll(tb, "vty sockets never appeared", func() bool {
		for _, d := range o.Daemons {
			if _, err := os.Stat(filepath.Join(run, d+".vty")); err != nil {
				return false
			}
		}

		return true
	})

	// Load the integrated configuration now that every daemon is
	// listening.
	if out, err := exec.Command(
		vtysh, "--vty_socket", run, "--config_dir", etc,
		"-f", filepath.Join(etc, "frr.conf"),
	).CombinedOutput(); err != nil {
		tb.Fatalf("failed to load FRR config: %v: %s", err, out)
	}

	return i
}

// Render executes the text/template at path with data.
func (Oracle) Render(tb testing.TB, path string, data any) []byte {
	tb.Helper()

	tmpl, err := template.ParseFiles(path)
	if err != nil {
		tb.Fatalf("failed to parse FRR config template: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		tb.Fatalf("failed to render FRR config: %v", err)
	}

	return buf.Bytes()
}

// addresses returns o's addresses with the defaults applied.
func (o Oracle) addresses() Addresses {
	if o.Addresses.Host == nil && o.Addresses.Peer == nil {
		return DefaultAddresses()
	}

	return o.Addresses
}

// instanceName derives a unique, filesystem-legal instance name from tb.
func (o Oracle) instanceName(tb testing.TB) string {
	return sanitize(fmt.Sprintf("%s-%s-%d", strings.ToLower(o.EnvPrefix), tb.Name(), os.Getpid()))
}

// saveLogs implements the log contract for one instance: logs land in
// $<EnvPrefix>_LOGDIR when it is set, and in the test output when the
// test failed. Logs are failure diagnostics only: assertions come from
// the code under test or from structured vtysh JSON.
func (o Oracle) saveLogs(tb testing.TB, name string, logs []byte) {
	tb.Helper()

	if dir := os.Getenv(o.EnvPrefix + "_LOGDIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(dir, name+".log"), logs, 0o644)
		}
	}

	if tb.Failed() {
		tb.Logf("%s logs:\n%s", name, logs)
	}
}

// An Instance is one running FRR instance.
type Instance struct {
	o                     Oracle
	name, vtysh, run, etc string
}

// Name returns the instance's name: the lowercased EnvPrefix, the test's
// name, and the process ID, joined by dashes, with every character
// outside [A-Za-z0-9] mapped to a dash. Saved logs are named for it.
func (i *Instance) Name() string { return i.name }

// Addresses returns the harness network's addresses, defaults applied.
func (i *Instance) Addresses() Addresses {
	a := i.o.addresses()
	return Addresses{
		Host: slices.Clone(a.Host),
		Peer: slices.Clone(a.Peer),
	}
}

// Vtysh invokes vtysh against the instance with each command as its own
// -c argument, returning stdout, joined by stderr only on failure.
// vtysh retains mode across -c arguments, so nested configuration lines
// work as they would interactively: configure terminal, then a router,
// then its statements.
func (i *Instance) Vtysh(cmds ...string) ([]byte, error) {
	args := []string{"--vty_socket", i.run, "--config_dir", i.etc}
	for _, cmd := range cmds {
		args = append(args, "-c", cmd)
	}

	out, err := exec.Command(i.vtysh, args...).Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			out = append(out, exit.Stderr...)
		}
	}

	return out, err
}

// JSON runs one vtysh command against the instance and unmarshals its
// JSON output into v, which may be nil to discard the output.
func (i *Instance) JSON(tb testing.TB, cmd string, v any) error {
	tb.Helper()

	out, err := i.Vtysh(cmd)
	if err != nil {
		return fmt.Errorf("vtysh %q: %w: %s", cmd, err, out)
	}

	if v == nil {
		return nil
	}

	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("vtysh %q: failed to unmarshal: %w: %s", cmd, err, out)
	}

	return nil
}

// Configure applies configuration lines through vtysh, entering
// configure terminal first, and fails tb if vtysh rejects them.
func (i *Instance) Configure(tb testing.TB, lines ...string) {
	tb.Helper()

	if out, err := i.Vtysh(append([]string{"configure terminal"}, lines...)...); err != nil {
		tb.Fatalf("failed to configure %s (%q): %v: %s", i.name, lines, err, out)
	}
}

// Poll waits for a condition of the instance within the oracle's
// timeout, naming the instance on failure.
func (i *Instance) Poll(tb testing.TB, msg string, fn func() bool) {
	tb.Helper()

	Poll(tb, cmp.Or(i.o.Timeout, DefaultTimeout), i.name+": "+msg, fn)
}

// Poll invokes fn every 250ms until it reports true, failing tb after
// timeout. Polling an external process or the kernel is the documented
// exception to the no-sleep rule: neither state machine is ours to
// synchronize on.
func Poll(tb testing.TB, timeout time.Duration, msg string, fn func() bool) {
	tb.Helper()

	deadline := time.Now().Add(timeout)
	for !fn() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out polling: %s", msg)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

// detect locates the FRR daemons when the environment does not name
// them, returning "" when none are found. Distro packages install the
// daemons off $PATH in a libexec-style directory. vtysh does land on
// $PATH, and its prefix signposts a relocated install, such as Nix, the
// dev shell, or /usr/local.
func detect(daemons []string) string {
	candidates := []string{"/usr/lib/frr", "/usr/libexec/frr"}
	if vtysh, err := exec.LookPath("vtysh"); err == nil {
		if vtysh, err = filepath.EvalSymlinks(vtysh); err == nil {
			prefix := filepath.Dir(filepath.Dir(vtysh))
			candidates = append(
				candidates,
				filepath.Join(prefix, "libexec", "frr"),
				filepath.Join(prefix, "lib", "frr"),
			)
		}
	}

next:
	for _, dir := range candidates {
		for _, d := range daemons {
			if _, err := os.Stat(filepath.Join(dir, d)); err != nil {
				continue next
			}
		}

		return dir
	}

	return ""
}

// findVtysh resolves vtysh for the daemons in dir. vtysh commonly lives
// apart from the daemons: beside them, in the install's own bin, reached
// as ../../bin from a directory such as libexec/frr, or on $PATH, such
// as /usr/bin for distro packages.
func findVtysh(dir string) (string, error) {
	for _, c := range []string{
		filepath.Join(dir, "vtysh"),
		filepath.Join(dir, "..", "..", "bin", "vtysh"),
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}

	vtysh, err := exec.LookPath("vtysh")
	if err != nil {
		return "", fmt.Errorf("vtysh not found beside %s or on $PATH: %w", dir, err)
	}

	return vtysh, nil
}

// sanitize maps every character of s outside [A-Za-z0-9] to a dash.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
}
