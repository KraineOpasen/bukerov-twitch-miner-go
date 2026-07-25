// Package boundary holds repository-level architectural enforcement tests.
//
// It has no runtime code. Its sole purpose is to host AST-based guards that fail
// the build when an architectural invariant is violated — currently, the
// BKM-021 environment-read boundary: production code must not call os.Getenv /
// os.LookupEnv outside the small, explicitly documented set of files that own
// the process's environment reads (the centralized startup resolver and the
// deferred subsystem-local secret boundaries).
//
// The enforcement lives in env_boundary_test.go. This file exists only so the
// directory is a normal, buildable package (rather than a test-only directory)
// so `go build ./...` and `go vet ./...` treat it uniformly.
package boundary
