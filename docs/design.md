# frrtest design

The design for `github.com/mdlayher/frrtest`, a shared FRR interop harness
extracted from the netns harnesses in `github.com/mdlayher/bfd`,
`github.com/mdlayher/bgp`, and `github.com/mdlayher/netlab/isis`. Those
three suites are the spec: what they share is the module, and what
differs between them stays in each consumer.

Drafted by frrtest-main on 2026-09-26 from a read of the three sources.
Every open question was put to Matt on 2026-09-27 and decided; the
decisions are listed at the end.

## What the sources share

The netns runtime is one program in three copies. Diffing the three
`netns.go` files leaves only these deltas:

- Renamed constants: the env prefix, the veth names, the run directory
  prefix, and the protocol daemon beside zebra.
- bgp writes `accept_ra=0` on the host veth before the link comes up.
- bfd adds a second IPv4 host address to the host veth.
- bgp starts bgpd with `-S` where the others start their daemon with
  `-u root -g root`. Both sidestep the missing frr user.
- Prose and blank-line style.

The driver in `frr.go` is textually the same across all three for the
vtysh JSON wrapper, the template rendering, the poll loop, `saveLogs`, and
`instanceName`. Every deadline in every harness is 60 seconds, except
bgp's establish wait, which is the consumer's own.

What the consumer tests touch on the `frr` handle, counted across all
three suites:

| Member | Uses | Module or consumer |
|---|---|---|
| `Addr`, `Addr6` | 39 | module: the instance's addresses |
| `awaitStatus`, `awaitEstablished`, `awaitAdjacency`, `awaitRoute`, `awaitPrefixPaths`, `awaitNotified` | 28 | consumer: protocol await helpers |
| `configure` | 3 | module |
| `vtysh` (JSON) | 2 | module |
| `poll` | 2 | module, and isis's host side uses the package-level one |
| `peer`, `neighbor`, `adjacencies`, `linkLocal` | 7 | consumer: JSON views |
| `raw` | 1 | module: `show version` for the tripwire test |

## Packages

Two packages from the start:

- `github.com/mdlayher/frrtest/netns` is the rig. It re-executes the test
  binary into a user and network namespace, hosts instances in nested
  namespaces, hands over a veth pair, and supervises daemons. It knows
  nothing about FRR. lasthopdev's `internal/lhns` is a future consumer.
- `github.com/mdlayher/frrtest` is the oracle on top of it: the daemon
  list, the version tripwire, vtysh, polling, config load, and logs.
  `doc.go` already declares this package.

Config templates, JSON view structs, await helpers, and the host side stay
in each consumer.

Neither package holds mutable state. The rig has no handle type: its
entry points are functions, the process's place in the scheme is carried
by the environment, and the lock which serializes instances is a file
the kernel holds. The oracle is a plain value a consumer declares once
and never writes.

## netns

```go
// A MainConfig configures Main.
type MainConfig struct {
    // Setup runs once in the original process, before it re-executes
    // into the namespaces. An error is fatal to the run. Environment it
    // sets reaches every process the rig starts.
    Setup func() error
}

// Main is the whole body of a package's TestMain. It never returns.
func Main(m *testing.M, cfg MainConfig)

// Start hosts one instance in fresh network, UTS, and mount namespaces,
// torn down when tb ends. Instances are serialized: Start blocks until
// the test which started the previous instance has finished, so tests
// may call t.Parallel.
func Start(tb testing.TB, cfg InstanceConfig) *Instance

// An InstanceConfig describes one instance.
type InstanceConfig struct {
    // Dir is the instance's run directory, created by the caller. The
    // init's log and each daemon's log land here.
    Dir string

    // Hostname pins the kernel hostname in the instance's UTS namespace.
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

    Path string
    Args []string
}

// An Instance is one running instance.
type Instance struct{ ... }

// Logs returns every daemon's log and the init's, each under a header.
func (i *Instance) Logs() []byte
```

The three hats stay, told apart by fixed environment markers
`FRRTEST_NETNS_CHILD` and `FRRTEST_NETNS_INIT`. These are per-process
plumbing and need no consumer prefix.

1. Hat one is `Main` in the process go test starts: run `Setup`,
   re-execute `/proc/self/exe` with `os.Args[1:]` into
   `CLONE_NEWUSER|CLONE_NEWNET` mapping the user to root, and relay the
   exit code.
2. Hat two is `Main` in that child: bring `lo` up, create the lock file,
   and run `m`. Each `Start` spawns hat three.
3. Hat three is the per-instance init, holding the instance's network,
   UTS, and mount namespaces and supervising its daemons.

The init is driven by a message, not by compiled-in constants: hat two
writes the `InstanceConfig` as one line of JSON on the init's stdin. The
init reads it, sets the hostname, applies the mounts and directories,
prints `ready`, and waits for `veth`. Hat two builds the pair, addresses
and raises its end, moves the peer into the init's namespace by pid, and
sends `veth`. The init addresses and raises the peer, starts the daemons,
prints `up`, and waits for stdin to close. Closing stdin is teardown: the
init kills the daemons and exits, dissolving the namespaces and the pair
with them. A ten second kill timer and an explicit `ip link del` of the
host end are backstops.

### The lock

The harness network has one veth pair with fixed names and one address
set, so instances cannot overlap. `Start` serializes them with a lock
the kernel holds rather than a mutex, so the module carries no mutable
state and nothing in the public API exists only to house one:

- Hat two creates an empty lock file in a directory of its own under
  `/tmp`, removed when `m` returns, and names it in `FRRTEST_NETNS_LOCK`
  for `Start` to find.
- `Start` opens the file and takes `flock(LOCK_EX)`, which blocks until
  the holder releases it. It registers the release, `LOCK_UN` and close,
  as its first cleanup, so it runs last, after the instance and
  everything the test started have torn down. This is the ordering bfd's
  mutex relies on, kept.
- Each `Start` opens its own descriptor, so each holds its own lock, and
  the kernel drops a lock whose process dies. A test which fails without
  reaching cleanup cannot wedge the next.

The deferral is recorded on the lock, in `Start`'s doc comment and in
the package doc: real parallelism means per-instance veth names and
subnets derived from the instance, and then this lock is deleted and
nothing else in the design changes.

The rig builds on Linux only. On other platforms `Main` runs `m` in
place and `Start` skips `tb`. A consumer which wants a hard failure on
other platforms keeps its own `main_others_test.go`, as all three do
today.

lasthopdev's `internal/lhns` will need two more fields on `MainConfig`
when it migrates: a fallback when the namespace cannot be created, which
runs the tests in-process and lets the ones needing the rig skip, and a
child-side setup hook for its single-namespace veth pair. Neither is in
scope now; this is where they go.

## frrtest

```go
// An Oracle describes the FRR instances a suite hosts. A consumer
// declares one at package level, calls Main on it from TestMain and
// Start on it from tests, and never writes it: it is a value, and it
// holds no state.
type Oracle struct {
    // Version is the FRR release the daemons must report. Main fails
    // the run on a mismatch. The module pins nothing: each consumer's
    // flake supplies its FRR and names its version here.
    Version string

    // Daemons names the daemons under the FRR directory, zebra first,
    // which every consumer requires. Each is started as
    // <dir>/<name> -u root -g root, running as the namespace's root
    // in place of the missing frr user. Tripwire names the one whose
    // --version enforces Version; it defaults to the last daemon.
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
    Addresses Addresses

    // Hostname is the instance's kernel hostname, DefaultHostname when
    // empty.
    Hostname string
}

// Addresses are the addresses on each end of the harness network.
type Addresses struct {
    Host, Peer []netip.Prefix
}

// DefaultAddresses returns the documentation prefixes: 192.0.2.1/24 and
// 2001:db8::1/64 on the host, 192.0.2.10/24 and 2001:db8::10/64 on the
// instance.
func DefaultAddresses() Addresses

const DefaultHostname = "frr-interop"

// Main is the whole body of a consumer's TestMain. It never returns.
func (o Oracle) Main(m *testing.M)

// Start starts an instance running the rendered configuration conf,
// torn down when tb ends. It returns once every daemon accepts
// configuration through vtysh; protocol readiness is the caller's poll.
// ctx bounds the wait for every daemon's vty socket before the
// configuration load.
func (o Oracle) Start(ctx context.Context, tb testing.TB, conf []byte) *Instance

// Render executes the text/template at path with data.
func (o Oracle) Render(tb testing.TB, path string, data any) []byte

// Poll invokes fn every 250ms until it returns nil, or until ctx ends,
// which is the failure: the returned error wraps ctx.Err() and fn's last
// error. Polling an external process or the kernel is the documented
// exception to the no-sleep rule: neither state machine is ours to
// synchronize on.
func Poll(ctx context.Context, fn func() error) error

// An Instance is one running FRR instance.
type Instance struct{ ... }

func (i *Instance) Name() string
func (i *Instance) Addresses() Addresses
func (i *Instance) Vtysh(cmds ...string) ([]byte, error)
func (i *Instance) JSON(tb testing.TB, cmd string, v any) error
func (i *Instance) Configure(tb testing.TB, lines ...string)
// Poll is Poll naming the instance in the error.
func (i *Instance) Poll(ctx context.Context, fn func() error) error
```

`Main` runs in the original process, before any namespace: when the env
does not name the FRR directory, detect it as the sources do, by statting
every name in `Daemons` under the distro paths and under the prefix of
`vtysh` on `$PATH`; stat every daemon; resolve `vtysh` beside the
daemons, in the install's bin, or on `$PATH`; run `<Tripwire> --version`
and compare the third field of its first line to `Version`. Any failure
is fatal, never a skip. Then hand off to `netns.Main`, with detection in
`Setup` so what it finds reaches every hat through the environment.

`Start` creates the run directory with `os.MkdirTemp("/tmp", ...)`, since
`sun_path` caps unix socket paths at 108 bytes and `t.TempDir` nests past
it; writes `frr.conf` from conf and `vtysh.conf` holding `service
integrated-vtysh-config`, which keeps vtysh from complaining about a
missing file; writes the run directory's group file as `/etc/group` with
`frrvty:x:0:` appended, since zebra insists on resolving that group and
gid 0 is the only one mapped; builds the `InstanceConfig` with tmpfs on
`/run` and `/var/lib`, the group file bound over `/etc/group`, `/run/frr`
and `/var/lib/frr` created, the veth from the oracle's names and
addresses, and each daemon as `<dir>/<name> -u root -g root --vty_socket
<run> -i /run/frr/<name>.pid -z /run/frr/zserv.api`; calls `netns.Start`;
polls for every daemon's `.vty` socket within ctx; loads the config with `vtysh
--vty_socket <run> --config_dir <etc> -f frr.conf`; and registers log
saving. Log saving reads the instance's logs before the rig's cleanup
tears it down, which cleanup's LIFO order gives when `Start` registers
after the rig, and writes them to `$<EnvPrefix>_LOGDIR/<name>.log` when
that is set and to `tb.Logf` when the test failed.

`vtysh.conf` therefore moves into the module. It is one identical line in
all three consumers' testdata, and each drops that file.

`Vtysh` passes each command as its own `-c`, so mode persists across
commands and nested configuration lines work as they would
interactively, and returns stdout with stderr appended on failure. `JSON`
runs one command and unmarshals into v, which may be nil to discard the
output. `Configure` prepends `configure terminal` and fails `tb` on
error. `Poll` calls fn once before waiting, so a condition which already
holds returns without a tick, then selects on `ctx.Done()` and a ticker,
so it stops as soon as ctx ends rather than sleeping past the deadline.
It returns an error and fails nothing: the caller bounds the wait with
`context.WithTimeout` or `t.Context()` and writes the failure message,
as in `if err := f.Poll(ctx, fn); err != nil { t.Fatalf("isisd never
answered vtysh: %v", err) }`. fn returns an error rather than a bool so
the failure carries why the condition never held, such as vtysh's
complaint. `Poll` on an instance names the instance in the error; the
package-level `Poll` is for the consumer's own waits outside any
instance, such as isis polling the kernel for the host veth's link-local
address.

Instance names are `<prefix>-<TestName>-<pid>` with every character
outside `[A-Za-z0-9]` mapped to `-`, as `instanceName` does today.

`Addresses` on the instance returns the resolved values, so a consumer on
the defaults never restates them. Both sides are lists because bfd puts
two IPv4 addresses on the host end.

## Consequences for the consumers

The isis migration, commit 13 of the isis move, is the acceptance test
for this design. After it, `interop/` keeps the template, `frrConfig`,
the JSON views, the await helpers, `host.go`, and a `TestMain` of one
line plus the `Oracle` literal, and drops `netns.go`, the generic half of
`frr.go` and `harness.go`, and `testdata/frr/vtysh.conf`. The harness
addresses move to the documentation prefixes, and the comments explaining
RFC 1918 and ULA space go with them.

bfd passes two host prefixes in `Addresses.Host` and drops `instanceMu`.
bgp confirms bgpd runs with `-u root -g root` in place of `-S`; if it
does not, `Daemons` becomes a slice of `netns.Daemon` and the consumer
supplies arguments.

## Self-test

The rig's tests need no FRR: a daemon can be the test binary itself or
`sleep`, and the tests prove the handoff, the mounts, the hostname pin,
teardown, and that two parallel tests calling `Start` never hold an
instance at the same time.

The oracle's tests need an FRR. The module's flake provides one for
development and CI only, which is not a pin: the module's `TestMain`
reads the version from `zebra --version` and passes it through, so the
flake's FRR can move without touching Go. The tripwire's failure path is
tested by re-executing the test binary with a wrong version and asserting
on the exit.

## Carry over unchanged

Each of these has a reason in the sources which the code alone does not
show. The implementer keeps each one with its comment:

- `ip` resolved through `EvalSymlinks` in the init before the tmpfs over
  `/run`, since NixOS `$PATH` entries resolve through it.
- `runtime.LockOSThread` in the init, so `Pdeathsig` watches a thread
  which lives as long as the process.
- `nodad` on every IPv6 address add.
- `accept_ra=0` written while the host link is still down, before any
  advertisement can arrive.
- The group file with `frrvty:x:0:` appended.
- The run directory under `/tmp`, not `t.TempDir()`.
- `os.Args[1:]` passed to the re-executed child so `-test.run`,
  `-test.v`, and friends reach the tests.
- Logs read in cleanup before the init's stdin is closed.
- The init's stderr to `init.log` in the run directory, and `expect`
  dumping it when the init never reports.

## Decisions

Matt decided each of these on 2026-09-26 and 2026-09-27, except where a
later date is given:

- Two packages from the start: rig below, oracle above.
- Parameterized by daemon list, env prefix, veth names, addresses with
  documentation-prefix defaults, and the tripwire binary. The host veth
  always gets `accept_ra=0`.
- Each consumer passes its own version; the module enforces it and pins
  none.
- Instances serialize behind an flock the child process creates and
  `Start` takes, until real parallelism with per-instance veth names and
  subnets is built. Tests may call `t.Parallel`. The deferral is
  recorded on the lock and in the rig's package doc. A mutex was
  considered and rejected because it needs a shared Go home, which is
  package-level mutable state wherever it is put.
- No handle types for the sake of state: `netns.Main` and `netns.Start`
  are functions, and `Oracle` is a plain value with no unexported
  fields. The CLAUDE.md rule against package-level mutable state stands
  with no exception.
- Internal env markers are fixed `FRRTEST_` names. The consumer's prefix
  is on the FRR directory and log directory variables only.
- Daemons are names only, always started with `-u root -g root`.
- `testing.TB` throughout, not `*testing.T`.
- The module's own tests take their version from the flake's FRR; the
  tripwire's failure path is tested by re-execution.
- Templates, JSON views, await helpers, and the host side stay in the
  consumer.
- Linux only at runtime; other platforms build and skip.
- On 2026-10-23: polls are bounded by the caller's context, not a fixed
  timeout. `Start` and both `Poll`s take a `context.Context`, `Poll`
  returns an error wrapping `ctx.Err()` and fn's last error instead of
  failing a `testing.TB`, and `Oracle.Timeout` and `DefaultTimeout`
  are gone. fn returns an error, and polling stops when it returns nil.
