// Package e2e hosts cross-package integration tests that span api + daemon.
//
// Living here avoids the import cycle that would arise if these tests lived
// inside internal/api — daemon already imports api, so api tests cannot
// import daemon.
//
// This package contains no production code; it only exists so Go's test
// binary has a compilation unit to attach the integration tests to.
package e2e
