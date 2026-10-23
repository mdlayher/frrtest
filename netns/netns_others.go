//go:build !linux

package netns

import (
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
