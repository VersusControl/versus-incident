// Package harnessendpoint is the AWS transport a test build sends every AWS
// call through, so a harness or gate image talks to a fake AWS server or an
// AWS API emulator instead of AWS. Every binary passes HTTPClient() as its AWS
// transport override (mount.Deps.HTTPClient).
//
// The redirect is compiled only with the harness_awsendpoint build tag (or
// its older alias harness_fakeaws). A release build has no redirect and no
// setting that turns one on: HTTPClient returns nil and AWS calls go to AWS.
package harnessendpoint
