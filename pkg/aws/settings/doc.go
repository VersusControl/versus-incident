// Package settings persists and serves the AWS connector configuration: the
// accounts a deployment reads, the health of each assumed role, the wizard
// drafts that become accounts, and the privacy self-test that proves no
// secret reaches the model.
//
// Records live in the shared storage.Provider under the exact keys
// aws/accounts/<account_id>, aws/drafts/<draft_id>, aws/health/<account_id>,
// and aws/privacy-selftest, and every access is checked against the
// deployment's tenancy scope. Config-defined accounts are never persisted;
// they are merged over stored accounts with config winning on the same ID.
package settings
