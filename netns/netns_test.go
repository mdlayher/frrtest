//go:build linux

package netns_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/frrtest/netns"
)

// The harness network these tests build. Start serializes instances, so
// one pair of names serves every test.
const (
	hostVeth = "nstest-host"
	peerVeth = "nstest-peer"
)

// probeArg, as the test binary's first argument, makes it a probe
// daemon: see probeMain.
const probeArg = "netns-probe"

// timeout bounds every wait on an instance.
const timeout = time.Minute

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == probeArg {
		probeMain(os.Args[2]) // never returns
	}

	netns.Main(m, netns.MainConfig{})
}

// TestStart proves an instance is the configuration it was given: its
// daemons see the pinned hostname, the mounts, the directories, and
// their end of the veth pair, and reach the test across it over both
// families. None of that leaks back into the test's namespace, and
// teardown takes the daemons and the pair with it.
func TestStart(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	tmp := t.TempDir()
	tmpfs := filepath.Join(tmp, "tmpfs")
	source := filepath.Join(tmp, "source")
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(tmpfs, 0o755); err != nil {
		t.Fatalf("failed to create tmpfs mount point: %v", err)
	}

	for file, b := range map[string]string{source: "bound\n", target: "unbound\n"} {
		if err := os.WriteFile(file, []byte(b), 0o644); err != nil {
			t.Fatalf("failed to write %s: %v", file, err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("failed to find the test binary: %v", err)
	}

	veth := testVeth()
	probe := func(name string, host netip.Prefix) netns.Daemon {
		b, err := json.Marshal(probeSpec{
			Name:  name,
			Dial:  net.JoinHostPort(host.Addr().String(), port),
			Tmpfs: tmpfs,
			Dir:   filepath.Join(tmpfs, "made"),
			Bind:  target,
			Peer:  peerVeth,
		})
		if err != nil {
			t.Fatalf("failed to encode probe: %v", err)
		}

		return netns.Daemon{
			Name: name,
			Path: exe,
			Args: []string{probeArg, string(b)},
		}
	}

	const hostname = "netns-instance"
	cfg := netns.InstanceConfig{
		Dir:      t.TempDir(),
		Hostname: hostname,
		Mounts: []netns.Mount{
			{
				Type:   "tmpfs",
				Target: tmpfs,
			},
			{
				Type:   "bind",
				Source: source,
				Target: target,
			},
		},
		Dirs: []string{filepath.Join(tmpfs, "made")},
		Veth: veth,
		Daemons: []netns.Daemon{
			probe("probe4", veth.HostAddrs[0]),
			probe("probe6", veth.HostAddrs[1]),
		},
	}

	// Registered before Start, this runs after the instance is gone.
	var pids []int
	t.Cleanup(func() {
		if _, err := net.InterfaceByName(hostVeth); err == nil {
			t.Errorf("%s outlived its instance", hostVeth)
		}

		for _, pid := range pids {
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("probe %d outlived its instance: %v", pid, err)
			}
		}
	})

	i := netns.Start(t, cfg)

	got := make(map[string]probeReport)
	lines := make(map[string][]byte)
	for range cfg.Daemons {
		line := accept(t, ln)

		var r probeReport
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("failed to decode a probe report %q: %v", line, err)
		}

		pids = append(pids, r.PID)
		r.PID = 0
		got[r.Name] = r
		lines[r.Name] = line
	}

	report := func(name string) probeReport {
		return probeReport{
			Name:     name,
			Hostname: hostname,
			Tmpfs:    true,
			Dir:      true,
			Bind:     "bound\n",
			Addrs:    []string{"192.0.2.10/24", "2001:db8::10/64"},
		}
	}

	want := map[string]probeReport{
		"probe4": report("probe4"),
		"probe6": report("probe6"),
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("unexpected probe reports (-want +got):\n%s", diff)
	}

	// Each probe logs its report before it dials, so the logs hold both
	// by now.
	logs := i.Logs()
	for name, line := range lines {
		if want := fmt.Sprintf("=== %s.log ===\n%s\n", name, line); !bytes.Contains(logs, []byte(want)) {
			t.Errorf("logs lack %s's report:\n%s", name, logs)
		}
	}

	if !bytes.Contains(logs, []byte("=== init.log ===\n")) {
		t.Errorf("logs lack the init's:\n%s", logs)
	}

	ifi, err := net.InterfaceByName(hostVeth)
	if err != nil {
		t.Fatalf("failed to find %s: %v", hostVeth, err)
	}

	addrs, err := globalAddrs(ifi)
	if err != nil {
		t.Fatalf("failed to list %s addresses: %v", hostVeth, err)
	}

	if diff := cmp.Diff([]string{"192.0.2.1/24", "2001:db8::1/64"}, addrs); diff != "" {
		t.Errorf("unexpected %s addresses (-want +got):\n%s", hostVeth, diff)
	}

	ra, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv6/conf", hostVeth, "accept_ra"))
	if err != nil {
		t.Fatalf("failed to read accept_ra: %v", err)
	}

	if got := string(ra); got != "0\n" {
		t.Errorf("unexpected %s accept_ra: %q", hostVeth, got)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("failed to read hostname: %v", err)
	}

	if host == hostname {
		t.Error("the instance's hostname leaked into the test's namespace")
	}

	if _, err := os.Stat(filepath.Join(tmpfs, "made")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the instance's directory leaked into the test's namespace: %v", err)
	}

	if b, err := os.ReadFile(target); err != nil || string(b) != "unbound\n" {
		t.Errorf("the instance's bind mount leaked into the test's namespace: %q, %v", b, err)
	}
}

// TestStartSerialized proves parallel tests take turns: however the
// scheduler interleaves them, no two ever hold an instance at once.
func TestStartSerialized(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	for i := range 3 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()

			netns.Start(t, netns.InstanceConfig{
				Dir:  t.TempDir(),
				Veth: testVeth(),
			})

			if n := active.Add(1); n != 1 {
				t.Errorf("%d instances held at once", n)
			}

			// Registered after Start, this runs before its teardown.
			t.Cleanup(func() { active.Add(-1) })
		})
	}
}

// TestStartInitFails proves an init which cannot build its instance
// fails the test with its own account of why, and releases the lock for
// the next instance.
func TestStartInitFails(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")
	tb := &fatalTB{TB: t}

	var wg sync.WaitGroup
	wg.Go(func() {
		netns.Start(tb, netns.InstanceConfig{
			Dir: t.TempDir(),
			Mounts: []netns.Mount{{
				Type:   "bind",
				Source: missing,
				Target: missing,
			}},
			Veth: testVeth(),
		})
	})
	wg.Wait()
	tb.cleanup()

	for _, want := range []string{`init never reported "ready"`, "mounting bind on " + missing} {
		if !strings.Contains(tb.fatal, want) {
			t.Errorf("failure %q lacks %q", tb.fatal, want)
		}
	}

	// The lock is free again: a failed Start must not wedge the next.
	netns.Start(t, netns.InstanceConfig{
		Dir:  t.TempDir(),
		Veth: testVeth(),
	})
}

// testVeth returns the harness network's veth pair.
func testVeth() netns.Veth {
	return netns.Veth{
		Host: hostVeth,
		Peer: peerVeth,
		HostAddrs: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.1/24"),
			netip.MustParsePrefix("2001:db8::1/64"),
		},
		PeerAddrs: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.10/24"),
			netip.MustParsePrefix("2001:db8::10/64"),
		},
	}
}

// accept returns everything the next connection to ln sends.
func accept(t *testing.T, ln net.Listener) []byte {
	t.Helper()

	if err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("failed to set accept deadline: %v", err)
	}

	c, err := ln.Accept()
	if err != nil {
		t.Fatalf("no probe reported: %v", err)
	}

	defer c.Close()

	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}

	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("failed to read a probe report: %v", err)
	}

	return bytes.TrimSuffix(b, []byte("\n"))
}

// globalAddrs returns the addresses on ifi other than link-local ones,
// which the kernel adds to every IPv6 link of its own accord.
func globalAddrs(ifi *net.Interface) ([]string, error) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}

	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.String())
		}
	}

	return out, nil
}

// A probeSpec tells a probe daemon what to look at inside its instance
// and where to report it.
type probeSpec struct {
	Name, Dial, Tmpfs, Dir, Bind, Peer string
}

// A probeReport is what a probe daemon saw inside its instance.
type probeReport struct {
	Name     string
	PID      int
	Hostname string

	// Tmpfs reports whether the spec's Tmpfs is a tmpfs, Dir whether its
	// Dir exists, and Bind the contents of its Bind.
	Tmpfs, Dir bool
	Bind       string

	// Addrs are the global addresses on the spec's Peer.
	Addrs []string
}

// probeMain is the test binary as a probe daemon: it inspects its
// instance as the spec in arg directs, logs the report, sends it to the
// test, and then waits to be killed. It never returns.
func probeMain(arg string) {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "probe: "+format+"\n", args...)
		os.Exit(1)
	}

	var spec probeSpec
	if err := json.Unmarshal([]byte(arg), &spec); err != nil {
		fail("decoding spec: %v", err)
	}

	host, err := os.Hostname()
	if err != nil {
		fail("reading hostname: %v", err)
	}

	// TMPFS_MAGIC, from linux/magic.h, which package syscall lacks.
	const tmpfsMagic = 0x01021994
	var st syscall.Statfs_t
	if err := syscall.Statfs(spec.Tmpfs, &st); err != nil {
		fail("statfs %s: %v", spec.Tmpfs, err)
	}

	_, dirErr := os.Stat(spec.Dir)
	bind, _ := os.ReadFile(spec.Bind)

	ifi, err := net.InterfaceByName(spec.Peer)
	if err != nil {
		fail("finding %s: %v", spec.Peer, err)
	}

	addrs, err := globalAddrs(ifi)
	if err != nil {
		fail("listing %s addresses: %v", spec.Peer, err)
	}

	line, err := json.Marshal(probeReport{
		Name:     spec.Name,
		PID:      os.Getpid(),
		Hostname: host,
		Tmpfs:    int64(st.Type) == tmpfsMagic,
		Dir:      dirErr == nil,
		Bind:     string(bind),
		Addrs:    addrs,
	})
	if err != nil {
		fail("encoding report: %v", err)
	}

	fmt.Printf("%s\n", line)

	c, err := net.DialTimeout("tcp", spec.Dial, timeout)
	if err != nil {
		fail("dialing %s: %v", spec.Dial, err)
	}

	if _, err := c.Write(append(line, '\n')); err != nil {
		fail("reporting: %v", err)
	}

	_ = c.Close()

	// The instance's teardown kills the probe, with SIGKILL, so there
	// is nothing to wait for but that.
	select {}
}

// A fatalTB is a testing.TB whose Fatal records the failure and ends
// the calling goroutine, and whose cleanups run when cleanup is called,
// so a test can observe a failure of the code it drives.
type fatalTB struct {
	testing.TB

	fatal    string
	cleanups []func()
}

func (tb *fatalTB) Helper()                      {}
func (tb *fatalTB) Cleanup(f func())             { tb.cleanups = append(tb.cleanups, f) }
func (tb *fatalTB) Fatal(args ...any)            { tb.fail(fmt.Sprint(args...)) }
func (tb *fatalTB) Fatalf(f string, args ...any) { tb.fail(fmt.Sprintf(f, args...)) }

// fail records msg and ends the calling goroutine, as testing.T's Fatal
// does.
func (tb *fatalTB) fail(msg string) {
	tb.fatal = msg
	runtime.Goexit()
}

// cleanup runs the registered cleanups, last first.
func (tb *fatalTB) cleanup() {
	for _, f := range slices.Backward(tb.cleanups) {
		f()
	}
}
