# frrtest [![Go Reference](https://pkg.go.dev/badge/github.com/mdlayher/frrtest.svg)](https://pkg.go.dev/github.com/mdlayher/frrtest)

Package `frrtest` runs FRRouting daemons in unprivileged Linux network
namespaces as an interoperability oracle for Go routing protocol tests.
Package `netns` beneath it is the rig: it hosts test instances in nested
namespaces joined to the test by a veth pair, and knows nothing about FRR.
The oracle's own tests need FRR, which the repository's nix dev shell
provides: `nix develop -c go test -race ./...`. MIT Licensed.
