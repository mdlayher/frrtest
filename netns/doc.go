// Package netns hosts test instances in nested Linux namespaces, joined
// to the test's own network namespace by a veth pair. It needs nothing
// beyond unprivileged user namespaces: no root, and no tools on $PATH,
// since links are configured over rtnetlink.
//
// A test binary using the rig wears three hats, told apart by
// environment markers which Main reads:
//
//  1. The process go test starts. Main runs MainConfig.Setup, then
//     re-executes the binary into a new user namespace, mapping the user
//     to root, plus a fresh network namespace for the harness network,
//     and relays its exit code.
//  2. The namespaced child, which runs the tests. Each Start spawns hat
//     three and hands it the veth peer.
//  3. A per-instance init, holding the instance's fresh network, UTS,
//     and mount namespaces and supervising its daemons, which die with
//     it through parent-death signals.
//
// The harness network is one veth pair with fixed names and one address
// set, so instances cannot overlap: Start serializes them behind a file
// lock the child creates. Real parallelism needs per-instance veth names
// and subnets derived from each instance; when those exist, the lock is
// deleted and nothing else changes.
//
// LinkUp and LinkDown set a link up or down in the caller's network
// namespace, such as the host end of an instance's veth pair, so a test
// can take the link away from the instance and give it back.
//
// The rig runs on Linux only. On other platforms Main runs the tests in
// place without Setup, Start skips, and LinkUp and LinkDown return an
// error.
package netns

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

// A MainConfig configures Main.
type MainConfig struct {
	// Setup runs once in the original process, before it re-executes
	// into the namespaces. An error is fatal to the run and is printed
	// as is. Environment it sets reaches every process the rig starts.
	Setup func() error
}

// An InstanceConfig describes one instance.
type InstanceConfig struct {
	// Dir is the instance's run directory, created by the caller. The
	// init's log and each daemon's log land here.
	Dir string

	// Hostname pins the kernel hostname in the instance's UTS namespace.
	// Empty leaves the hostname the namespace inherits.
	Hostname string

	// Mounts are applied in order in the instance's mount namespace,
	// after / is remounted private. Dirs are then created.
	Mounts []Mount
	Dirs   []string

	// Veth is the pair joining the test's namespace to the instance's.
	Veth Veth

	// Daemons are started in order under the init, each with a
	// parent-death signal and its output in <Dir>/<Name>.log.
	Daemons []Daemon
}

// A Mount is one mount in an instance's mount namespace.
type Mount struct {
	// Type is "tmpfs" or "bind". Source is unused for tmpfs.
	Type, Source, Target string
}

// A Veth is the pair joining the test's namespace to an instance's.
type Veth struct {
	// Host is the end which stays in the test's namespace and Peer the
	// end which moves into the instance's.
	Host, Peer string

	// HostAddrs and PeerAddrs are assigned to each end. IPv6 addresses
	// are added with nodad. The host end always has accept_ra disabled
	// before it comes up, so an advertisement from the instance can
	// never outrank the static addresses.
	HostAddrs, PeerAddrs []netip.Prefix
}

// A Daemon is one process an instance's init starts and supervises.
type Daemon struct {
	// Name is the stem of the daemon's log file.
	Name string

	// Path and Args are the daemon's executable and its arguments.
	Path string
	Args []string
}

// An Instance is one running instance.
type Instance struct {
	cfg InstanceConfig
}

// Logs returns every daemon's log and the init's, each under a header.
// Read them before the instance is torn down: a cleanup registered
// after Start runs first.
func (i *Instance) Logs() []byte {
	var logs bytes.Buffer
	for _, d := range i.cfg.Daemons {
		appendLog(&logs, i.cfg.Dir, d.Name+".log")
	}

	appendLog(&logs, i.cfg.Dir, "init.log")
	return logs.Bytes()
}

// appendLog appends the log file in dir to logs under a header. A
// missing file appears as an empty log.
func appendLog(logs *bytes.Buffer, dir, file string) {
	b, _ := os.ReadFile(filepath.Join(dir, file))
	fmt.Fprintf(logs, "=== %s ===\n%s", file, b)
}
