// Package awsemulator holds contract tests that run Versus's own AWS code
// (the account check, the permission probes and every collector) against a
// seeded AWS Emulator, and assert what Versus stores: resource IDs, relations,
// tags with the default tag-value allowlist applied, and CloudTrail changes.
//
// The tests build only with the awsemulator tag, and every test skips unless
// the test-build AWS endpoint redirect (the endpoint URL and CA bundle
// variables in cmd/harness_awsendpoint.go) names a running emulator, so plain
// `go test ./...` never needs it. This file carries no build tag so the
// package always exists; it must not name those variables, because the
// release-build guard in cmd scans every untagged file for them.
//
// The aws-emulator CI job starts the emulator, runs seed.sh in this folder,
// and then runs
//
//	go test -tags awsemulator ./tests/awsemulator/
//
// with the redirect and the emulator's credentials (an access key pair in the
// environment, or a shared credentials file) set. Tests that cover APIs the
// emulator cannot answer faithfully skip with the reason; tests/fakeaws
// covers those.
package awsemulator
