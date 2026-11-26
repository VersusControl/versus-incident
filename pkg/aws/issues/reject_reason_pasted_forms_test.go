package issues

import (
	"strings"
	"testing"
)

// The credential test values; leakedFragment looks for any 8-character
// run of them in a stored reason, so a half-redacted key still fails.
const (
	pastedKeyID   = "AKIAIOSFODNN7EXAMPLE"
	pastedTempID  = "ASIAY34FZKBOKMUTVV7A"
	pastedSecret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	pastedSession = "IQoJb3JpZ2luX2VjEJr//////////wEaCXVzLWVhc3QtMSJHMEUCIQD+SESSIONTOKENVALUE="
)

func leakedFragment(stored string) string {
	const run = 8
	for _, secret := range []string{pastedKeyID, pastedTempID, pastedSecret, pastedSession} {
		for i := 0; i+run <= len(secret); i++ {
			if frag := secret[i : i+run]; strings.Contains(stored, frag) && strings.Trim(frag, "/") != "" {
				return frag
			}
		}
	}
	return ""
}

// A pasted AWS credential is cut from the stored reason and its event in
// the other shapes people paste it in: inside a URL query, wrapped over a
// line break, quoted or in backticks, in a Markdown code fence, with a
// mixed-case label, and in CLI or STS JSON.
func TestRejectReasonRedactsAWSKeysInOtherPastedForms(t *testing.T) {
	open := Issue{ID: IssueID("flow-public-bucket", docsBucket), State: StateOpen, Severity: SeverityHigh, Digest: "d"}
	for name, reason := range map[string]string{
		"presigned url query": "public on purpose, link https://flow-docs.s3.amazonaws.com/a.pdf?AWSAccessKeyId=" + pastedKeyID +
			"&Signature=abc%2Bdef&x-amz-security-token=" + pastedSession,
		"sigv4 url query": "public on purpose https://flow-docs.s3.eu-west-1.amazonaws.com/a.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" +
			pastedKeyID + "%2F20261125%2Feu-west-1%2Fs3%2Faws4_request&X-Amz-Signature=0123abcd",
		"url query without a scheme":       "public on purpose flow-docs.s3.amazonaws.com/a.pdf?AWSAccessKeyId=" + pastedKeyID + "&Expires=1",
		"secret wrapped over a line break": "public on purpose\naws_secret_access_key = wJalrXUtnFEMI/K7MDENG\n/bPxRfiCYEXAMPLEKEY",
		"key id wrapped over a line break": "public on purpose, the key is AKIAIOSFOD\nNN7EXAMPLE",
		"single quotes":                    "public on purpose: aws_secret_access_key='" + pastedSecret + "' aws_access_key_id='" + pastedKeyID + "'",
		"double quotes":                    `public on purpose: AWS_SECRET_ACCESS_KEY="` + pastedSecret + `"`,
		"quoted label":                     `public on purpose: "aws_secret_access_key" = "` + pastedSecret + `"`,
		"backticks":                        "public on purpose, key `" + pastedKeyID + "` and secret key: `" + pastedSecret + "`",
		"markdown ini fence": "public on purpose\n```ini\n[default]\naws_access_key_id=" + pastedKeyID +
			"\naws_secret_access_key=" + pastedSecret + "\naws_session_token=" + pastedSession + "\n```",
		"markdown json fence": "public on purpose\n```json\n{\n  \"AccessKeyId\": \"" + pastedTempID + "\",\n  \"SecretAccessKey\": \"" +
			pastedSecret + "\",\n  \"SessionToken\": \"" + pastedSession + "\"\n}\n```",
		"fence on one line": "public on purpose ```" + pastedKeyID + " aws_secret_access_key=" + pastedSecret + "```",
		"mixed case labels": "public on purpose Aws_Secret_Access_Key = " + pastedSecret + "\nAws_Session_Token=" + pastedSession,
		"title case prose":  "public on purpose, Secret Access Key: " + pastedSecret,
		"upper case prose":  "public on purpose, SECRET KEY: " + pastedSecret,
		"compact cli json":  `public on purpose {"AccessKeyId":"` + pastedKeyID + `","SecretAccessKey":"` + pastedSecret + `"}`,
		"sts credentials json": `public on purpose {"Credentials": {"AccessKeyId": "` + pastedTempID + `", "SecretAccessKey": "` + pastedSecret +
			`", "SessionToken": "` + pastedSession + `", "Expiration": "2026-11-25T10:00:00Z"}}`,
		"secret before key id json": `public on purpose {"SecretAccessKey": "` + pastedSecret + `", "AccessKeyId": "` + pastedKeyID + `"}`,
		"powershell env":            `public on purpose $env:AWS_SECRET_ACCESS_KEY="` + pastedSecret + `"`,
		"yaml":                      "public on purpose\nenv:\n  aws_secret_access_key: " + pastedSecret + "\n  aws_access_key_id: " + pastedKeyID,
	} {
		t.Run(name, func(t *testing.T) {
			rejected, events, err := Reject(open, "ana@example.com", reason, "", start)
			if err != nil {
				t.Fatalf("reject = %v", err)
			}
			for _, stored := range []string{rejected.Rejection.Reason, events[0].Detail} {
				if frag := leakedFragment(stored); frag != "" {
					t.Fatalf("stored reason keeps credential text %q: %q", frag, stored)
				}
				if !strings.Contains(stored, "public on purpose") {
					t.Fatalf("redaction removed the reason itself: %q", stored)
				}
			}
		})
	}
}

// Ways of handing over a secret access key without '=' or ':' after its
// label: the aws configure set command and a sentence.
func TestRejectReasonRedactsAWSSecretWithoutSeparator(t *testing.T) {
	open := Issue{ID: IssueID("flow-public-bucket", docsBucket), State: StateOpen, Severity: SeverityHigh, Digest: "d"}
	for name, reason := range map[string]string{
		"aws configure set": "public on purpose, ran aws configure set aws_secret_access_key " + pastedSecret,
		"secret key is":     "public on purpose, the secret key is " + pastedSecret,
	} {
		t.Run(name, func(t *testing.T) {
			rejected, events, err := Reject(open, "ana@example.com", reason, "", start)
			if err != nil {
				t.Fatalf("reject = %v", err)
			}
			for _, stored := range []string{rejected.Rejection.Reason, events[0].Detail} {
				if frag := leakedFragment(stored); frag != "" {
					t.Fatalf("stored reason keeps credential text %q: %q", frag, stored)
				}
			}
		})
	}
}
