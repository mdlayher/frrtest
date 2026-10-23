//go:build !linux

package netns

import (
	"errors"
	"os"
	"testing"
)

// Main is the whole body of a package's TestMain. It never returns.
// Off Linux it runs the tests in place, without Setup.
func Main(m *testing.M, _ MainConfig) {
	os.Exit(m.Run())
}

// Start hosts one instance in fresh network, UTS, and mount namespaces.
// Off Linux it skips tb.
func Start(tb testing.TB, _ InstanceConfig) *Instance {
	tb.Helper()

	tb.Skip("netns: instances require Linux")
	return nil
}

// LinkUp sets the link named name administratively up in the caller's
// network namespace. Off Linux it returns an error.
func LinkUp(name string) error {
	return errors.New("netns: links require Linux")
}

// LinkDown sets the link named name administratively down in the
// caller's network namespace. The kernel flushes a link's IPv6
// addresses as it goes down unless its keep_addr_on_down is set, which
// Start sets on the host end of an instance's pair, so that end keeps
// its addresses across a down and up. Off Linux it returns an error.
func LinkDown(name string) error {
	return errors.New("netns: links require Linux")
}
