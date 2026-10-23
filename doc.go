// Package frrtest runs FRRouting daemons in unprivileged Linux network
// namespaces as an interoperability oracle for Go routing protocol tests.
//
// A suite declares an Oracle at package level, in whichever of its files
// start instances, and hands TestMain to it:
//
//	var oracle = frrtest.Oracle{
//		Version:   "10.7.0",
//		Daemons:   []string{"zebra", "isisd"},
//		EnvPrefix: "ISIS_INTEROP",
//		Host:      "isisi-host",
//		Peer:      "isisi-frr",
//	}
//
//	func TestMain(m *testing.M) { oracle.Main(m) }
//
// Each test then starts an instance from its own rendered configuration,
// waits for the protocol, and drives the instance through vtysh:
//
//	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
//	defer cancel()
//
//	f := oracle.Start(ctx, t, oracle.Render(t, "testdata/frr.conf.tmpl", cfg))
//	if err := f.Poll(ctx, func() error {
//		return f.JSON(t, "show isis summary json", nil)
//	}); err != nil {
//		t.Fatalf("isisd never answered vtysh: %v", err)
//	}
//
// The caller's context bounds every wait: Start's for the daemons, and
// each Poll's for its condition. A poll whose context ends returns an
// error wrapping ctx.Err() and the condition's last error, and the
// caller says what failed.
//
// The instance runs the daemons on the far end of a veth pair whose near
// end is in the test's own network namespace, so code under test speaks
// to FRR over a real link. Package netns provides the namespaces.
//
// $<EnvPrefix>_FRR names the daemon directory, such as /usr/lib/frr or a
// Nix store path's libexec/frr. Without it, Main looks in the usual
// install locations and beside vtysh on $PATH. A missing or mismatched
// FRR is a hard failure, never a skip.
//
// When $<EnvPrefix>_LOGDIR is set, each instance's logs are saved there
// on teardown; a failed test additionally logs them.
//
// FRR instances need Linux. On other platforms the package builds, Main
// runs the tests in place, and Start skips.
package frrtest
