//go:build linux

package netns

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jsimonetti/rtnetlink/v2"
	"github.com/jsimonetti/rtnetlink/v2/driver"
)

// Environment markers wiring the rig's re-executions. They are
// per-process plumbing, not settings.
const (
	envChild = "FRRTEST_NETNS_CHILD"
	envInit  = "FRRTEST_NETNS_INIT"
	envLock  = "FRRTEST_NETNS_LOCK"
)

// Main is the whole body of a package's TestMain. It never returns.
func Main(m *testing.M, cfg MainConfig) {
	switch {
	case os.Getenv(envInit) != "":
		initMain() // never returns
	case os.Getenv(envChild) != "":
		os.Exit(child(m))
	}

	if cfg.Setup != nil {
		if err := cfg.Setup(); err != nil {
			log.Fatal(err)
		}
	}

	// Re-enters Main as the namespaced child above.
	os.Exit(reexec())
}

// reexec is hat one: re-execute the test binary inside a fresh
// user+network namespace and relay its verdict.
func reexec() int {
	// os.Args[1:] carries -test.run, -test.v, and friends through to the
	// child, which is the process that actually runs the tests.
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), envChild+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		// Root inside the namespace is this user outside: enough to
		// create veth pairs and nested namespaces, and nothing more.
		UidMappings: []syscall.SysProcIDMap{{
			ContainerID: 0,
			HostID:      os.Getuid(),
			Size:        1,
		}},
		GidMappings: []syscall.SysProcIDMap{{
			ContainerID: 0,
			HostID:      os.Getgid(),
			Size:        1,
		}},
	}

	err := cmd.Run()
	if err == nil {
		return 0
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}

	log.Printf("netns: failed to re-execute into a user namespace: %v", err)
	return 1
}

// child is hat two: prepare the harness network and the instance lock,
// then run the tests.
func child(m *testing.M) int {
	// The re-exec landed in an empty network namespace; even loopback
	// starts down.
	if err := LinkUp("lo"); err != nil {
		log.Print(err)
		return 1
	}

	dir, err := os.MkdirTemp("/tmp", "frrtest-netns")
	if err != nil {
		log.Printf("netns: failed to create lock directory: %v", err)
		return 1
	}

	defer os.RemoveAll(dir)

	lock := filepath.Join(dir, "lock")
	f, err := os.Create(lock)
	if err != nil {
		log.Printf("netns: failed to create lock file: %v", err)
		return 1
	}

	_ = f.Close()

	if err := os.Setenv(envLock, lock); err != nil {
		log.Printf("netns: failed to name the lock file: %v", err)
		return 1
	}

	return m.Run()
}

// Start hosts one instance in fresh network, UTS, and mount namespaces,
// torn down when tb ends. It returns once every daemon has been
// started; whether a daemon is ready is the caller's to find out.
//
// Instances are serialized: Start blocks until the test which started
// the previous instance has finished, so tests may call t.Parallel. The
// lock is held until every other cleanup of tb has run, so everything
// the test started has torn down before the next instance starts. A
// test may therefore host only one instance: a second Start from the
// same test waits for itself forever. The lock exists because the veth
// names and addresses are fixed; instances with names and subnets of
// their own would let it go.
//
// Start must run under Main.
func Start(tb testing.TB, cfg InstanceConfig) *Instance {
	tb.Helper()

	// Registered first, the release runs last.
	lock(tb)

	// A race-enabled binary sleeps for a second on exit by default, in
	// case another thread has a race to report. The init runs no tests
	// and exits with the lock held, so that second would be added to
	// every instance's teardown.
	initCmd := exec.Command("/proc/self/exe")
	initCmd.Env = append(
		os.Environ(),
		envInit+"=1",
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"),
	)
	initCmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS,
	}

	stdin, err := initCmd.StdinPipe()
	if err != nil {
		tb.Fatalf("failed to open init stdin: %v", err)
	}

	stdout, err := initCmd.StdoutPipe()
	if err != nil {
		tb.Fatalf("failed to open init stdout: %v", err)
	}

	initLog, err := os.Create(filepath.Join(cfg.Dir, "init.log"))
	if err != nil {
		tb.Fatalf("failed to create init log: %v", err)
	}

	initCmd.Stderr = initLog

	err = initCmd.Start()
	_ = initLog.Close()
	if err != nil {
		tb.Fatalf("failed to start init: %v", err)
	}

	tb.Cleanup(func() {
		// Closing stdin tells init to take the daemons down and exit,
		// dissolving the namespaces and with them the veth pair; the
		// timer and the explicit delete are backstops.
		_ = stdin.Close()
		timer := time.AfterFunc(10*time.Second, func() { _ = initCmd.Process.Kill() })
		_ = initCmd.Wait()
		timer.Stop()
		_ = linkDelete(cfg.Veth.Host)
	})

	if err := json.NewEncoder(stdin).Encode(cfg); err != nil {
		tb.Fatalf("failed to send init its config: %v", err)
	}

	sc := bufio.NewScanner(stdout)
	expect := func(want string) {
		tb.Helper()

		if !sc.Scan() {
			b, _ := os.ReadFile(filepath.Join(cfg.Dir, "init.log"))
			tb.Fatalf("init never reported %q: %s", want, b)
		}

		if got := sc.Text(); got != want {
			tb.Fatalf("unexpected report from init: got %q, want %q", got, want)
		}
	}

	// The handoff: once init's namespaces exist, build the veth pair
	// here, push one end into them, and let init take it from there.
	expect("ready")
	if err := vethAdd(cfg.Veth.Host, cfg.Veth.Peer); err != nil {
		tb.Fatal(err)
	}

	for _, p := range cfg.Veth.HostAddrs {
		if err := addrAdd(cfg.Veth.Host, p); err != nil {
			tb.Fatal(err)
		}
	}

	// The harness network is statically addressed, so this side must
	// not autoconfigure. A router in the instance advertises the prefix
	// on its end of the pair, and a global address derived from that
	// advertisement outranks the static one as the source for
	// connections to the instance (RFC 6724, rule 5.5: prefer an address
	// whose prefix the selected next hop advertised). The instance then
	// rejects those connections when its configuration names the static
	// address. The knob is set while the link is still down, before any
	// advertisement can arrive, and its path names an interface which
	// exists only in this namespace.
	if err := os.WriteFile(filepath.Join("/proc/sys/net/ipv6/conf", cfg.Veth.Host, "accept_ra"), []byte("0\n"), 0o644); err != nil {
		tb.Fatalf("failed to disable IPv6 router advertisements on %s: %v", cfg.Veth.Host, err)
	}

	// The harness network is statically addressed on both ends, and a
	// LinkDown must not change that: by default the kernel flushes a
	// link's IPv6 addresses as it goes down, which would leave the host
	// end with only its IPv4 address after a bounce.
	if err := os.WriteFile(filepath.Join("/proc/sys/net/ipv6/conf", cfg.Veth.Host, "keep_addr_on_down"), []byte("1\n"), 0o644); err != nil {
		tb.Fatalf("failed to keep IPv6 addresses on %s across link down: %v", cfg.Veth.Host, err)
	}

	if err := LinkUp(cfg.Veth.Host); err != nil {
		tb.Fatal(err)
	}

	if err := linkSetNS(cfg.Veth.Peer, initCmd.Process.Pid); err != nil {
		tb.Fatal(err)
	}

	if _, err := fmt.Fprintln(stdin, "veth"); err != nil {
		tb.Fatalf("failed to hand init the veth peer: %v", err)
	}

	expect("up")
	return &Instance{cfg: cfg}
}

// lock takes the instance lock for tb, released when tb ends: see
// Start.
func lock(tb testing.TB) {
	tb.Helper()

	path := os.Getenv(envLock)
	if path == "" {
		tb.Fatal("netns: Start requires TestMain to call netns.Main")
	}

	// Each Start opens its own descriptor, so each holds its own lock,
	// and the kernel drops a lock whose process dies: a test which fails
	// without reaching cleanup cannot wedge the next.
	f, err := os.Open(path)
	if err != nil {
		tb.Fatalf("failed to open the instance lock: %v", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		tb.Fatalf("failed to take the instance lock: %v", err)
	}

	tb.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}

// LinkUp sets the link named name administratively up in the caller's
// network namespace.
func LinkUp(name string) error {
	if err := linkSetFlags(name, syscall.IFF_UP); err != nil {
		return fmt.Errorf("netns: failed to set %s up: %w", name, err)
	}

	return nil
}

// LinkDown sets the link named name administratively down in the
// caller's network namespace. The kernel flushes a link's IPv6
// addresses as it goes down unless its keep_addr_on_down is set, which
// Start sets on the host end of an instance's pair, so that end keeps
// its addresses across a down and up.
func LinkDown(name string) error {
	if err := linkSetFlags(name, 0); err != nil {
		return fmt.Errorf("netns: failed to set %s down: %w", name, err)
	}

	return nil
}

// linkSetFlags sets the IFF_UP bit of the link named name to the one in
// flags, leaving its other flags alone.
func linkSetFlags(name string, flags uint32) error {
	return withLink(name, func(c *rtnetlink.Conn, index uint32) error {
		return c.Link.Set(&rtnetlink.LinkMessage{
			Index:  index,
			Flags:  flags,
			Change: syscall.IFF_UP,
		})
	})
}

// vethAdd creates the veth pair host and peer in the caller's network
// namespace, both ends down.
func vethAdd(host, peer string) error {
	c, err := rtnetlink.Dial(nil)
	if err != nil {
		return fmt.Errorf("netns: failed to dial rtnetlink: %w", err)
	}

	defer c.Close()

	err = c.Link.New(&rtnetlink.LinkMessage{
		Attributes: &rtnetlink.LinkAttributes{
			Name: host,
			Info: &rtnetlink.LinkInfo{
				Kind: "veth",
				Data: &driver.Veth{
					PeerInfo: &rtnetlink.LinkMessage{
						Attributes: &rtnetlink.LinkAttributes{Name: peer},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("netns: failed to create veth pair %s and %s: %w", host, peer, err)
	}

	return nil
}

// addrAdd assigns p to the link named dev. IPv6 addresses skip
// duplicate address detection, which would otherwise hold each one
// tentative, and unusable, for a while after the link comes up: the
// pair has only two ends, and their addresses are fixed.
func addrAdd(dev string, p netip.Prefix) error {
	family, flags := uint8(syscall.AF_INET), uint32(0)
	if p.Addr().Is6() {
		family, flags = syscall.AF_INET6, syscall.IFA_F_NODAD
	}

	err := withLink(dev, func(c *rtnetlink.Conn, index uint32) error {
		return c.Address.New(&rtnetlink.AddressMessage{
			Family:       family,
			PrefixLength: uint8(p.Bits()),
			Scope:        syscall.RT_SCOPE_UNIVERSE,
			Index:        index,
			Attributes: &rtnetlink.AddressAttributes{
				Address: p.Addr().AsSlice(),
				Local:   p.Addr().AsSlice(),
				Flags:   flags,
			},
		})
	})
	if err != nil {
		return fmt.Errorf("netns: failed to add %s to %s: %w", p, dev, err)
	}

	return nil
}

// linkSetNS moves the link named name into the network namespace of the
// process pid.
func linkSetNS(name string, pid int) error {
	err := withLink(name, func(c *rtnetlink.Conn, index uint32) error {
		return c.Link.Set(&rtnetlink.LinkMessage{
			Index: index,
			Attributes: &rtnetlink.LinkAttributes{
				NetNS: rtnetlink.NetNSForPID(uint32(pid)),
			},
		})
	})
	if err != nil {
		return fmt.Errorf("netns: failed to move %s to the namespace of pid %d: %w", name, pid, err)
	}

	return nil
}

// linkDelete deletes the link named name.
func linkDelete(name string) error {
	err := withLink(name, func(c *rtnetlink.Conn, index uint32) error {
		return c.Link.Delete(index)
	})
	if err != nil {
		return fmt.Errorf("netns: failed to delete %s: %w", name, err)
	}

	return nil
}

// withLink calls fn with a fresh rtnetlink connection, closed when fn
// returns, and the index of the link named name in the caller's network
// namespace.
func withLink(name string, fn func(c *rtnetlink.Conn, index uint32) error) error {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}

	c, err := rtnetlink.Dial(nil)
	if err != nil {
		return err
	}

	defer c.Close()

	return fn(c, uint32(ifi.Index))
}

// initMain is hat three: a tiny init holding the namespaces of one
// instance. It never returns.
func initMain() {
	// The daemons' parent-death signals watch the thread that spawned
	// them; pinning the main goroutine keeps that thread alive for the
	// life of the process.
	runtime.LockOSThread()

	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "init: "+format+"\n", args...)
		os.Exit(1)
	}

	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		fail("did not receive a config: %v", sc.Err())
	}

	var cfg InstanceConfig
	if err := json.Unmarshal(sc.Bytes(), &cfg); err != nil {
		fail("decoding config: %v", err)
	}

	if cfg.Hostname != "" {
		if err := syscall.Sethostname([]byte(cfg.Hostname)); err != nil {
			fail("sethostname: %v", err)
		}
	}

	// A private root keeps the mounts below inside this namespace.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail("remounting / private: %v", err)
	}

	for _, m := range cfg.Mounts {
		var err error
		switch m.Type {
		case "tmpfs":
			err = syscall.Mount("tmpfs", m.Target, "tmpfs", 0, "")
		case "bind":
			err = syscall.Mount(m.Source, m.Target, "", syscall.MS_BIND, "")
		default:
			fail("unknown mount type %q for %s", m.Type, m.Target)
		}
		if err != nil {
			fail("mounting %s on %s: %v", m.Type, m.Target, err)
		}
	}

	for _, dir := range cfg.Dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("mkdir %s: %v", dir, err)
		}
	}

	if err := LinkUp("lo"); err != nil {
		fail("%v", err)
	}

	// Hat two hands the veth peer over once we are visible.
	fmt.Println("ready")
	if !sc.Scan() || sc.Text() != "veth" {
		fail("did not receive the veth handoff: %q", sc.Text())
	}

	for _, p := range cfg.Veth.PeerAddrs {
		if err := addrAdd(cfg.Veth.Peer, p); err != nil {
			fail("%v", err)
		}
	}

	if err := LinkUp(cfg.Veth.Peer); err != nil {
		fail("%v", err)
	}

	var daemons []*exec.Cmd
	for _, d := range cfg.Daemons {
		logf, err := os.Create(filepath.Join(cfg.Dir, d.Name+".log"))
		if err != nil {
			fail("creating %s log: %v", d.Name, err)
		}

		cmd := exec.Command(d.Path, d.Args...)
		cmd.Stdout, cmd.Stderr = logf, logf
		// The backstop: the daemon dies with this process even if the
		// explicit kills below never run.
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			fail("starting %s: %v", d.Name, err)
		}

		_ = logf.Close()
		daemons = append(daemons, cmd)
	}

	fmt.Println("up")

	// Hold the namespaces until hat two closes our stdin, then take the
	// daemons down with us.
	for sc.Scan() {
	}

	for _, d := range daemons {
		_ = d.Process.Kill()
		_ = d.Wait()
	}

	os.Exit(0)
}
