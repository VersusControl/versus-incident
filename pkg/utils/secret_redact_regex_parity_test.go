package utils

import (
	"regexp"
	"strings"
	"testing"
)

// regexParamRE and regexRedactSecrets are the name=value redactor that
// RedactSecrets has always run as its first pass, kept here so the full
// redactor (that pass, then the AWS pass) is held to everything the regex
// redacted.
var regexParamRE = regexp.MustCompile(`(?i)\b(sig|signature|token|access_token|key|api_key|apikey|password|secret|code)=[^&\s"'<>]+`)

// Tokens in the shape of a real one are built from parts, so the source holds
// no literal a secret scanner reports as a leaked credential.
var (
	testGitHubToken = "ghp_" + "examplepersonaltoken"
	testJWT         = "eyJhbGciOiJIUzI1NiJ9" + ".eyJzdWIiOiIxIn0.abc"
	testJWTExpiry   = "eyJhbGciOiJIUzI1NiJ9" + ".eyJleHAiOjF9.sig"
	testPEMBegin    = "-----BEGIN RSA " + "PRIVATE KEY-----"
)

func regexRedactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = urlInTextRE.ReplaceAllStringFunc(s, redactURL)
	s = botTokenRE.ReplaceAllString(s, "bot"+redacted)
	return regexParamRE.ReplaceAllString(s, "${1}="+redacted)
}

// regexRedactedValues returns the values the regex redactor cut from s
// and that no longer appear anywhere in its output.
func regexRedactedValues(s string) []string {
	s = urlInTextRE.ReplaceAllStringFunc(s, redactURL)
	s = botTokenRE.ReplaceAllString(s, "bot"+redacted)
	out := regexParamRE.ReplaceAllString(s, "${1}="+redacted)
	var values []string
	for _, m := range regexParamRE.FindAllStringSubmatch(s, -1) {
		value := m[0][len(m[1])+1:]
		if value != redacted && !strings.Contains(out, value) {
			values = append(values, value)
		}
	}
	return values
}

// redactionParityCorpus is realistic text the shared redactor sees:
// Kubernetes and application logs, delivery errors, environment dumps,
// connection strings, JWTs, JSON bodies and PEM headers. secrets are the
// values the regex redactor removed; keep is text both must leave alone.
var redactionParityCorpus = []struct {
	name    string
	in      string
	secrets []string
	keep    []string
}{
	{"kube log token param", `2026-11-25T09:00:00Z level=info msg="refresh ok" token=eyJhbGciOiJSUzI1NiJ9.e30.c2ln pod=api-7f9c`, []string{"eyJhbGciOiJSUzI1NiJ9.e30.c2ln"}, []string{"pod=api-7f9c", `msg="refresh ok"`}},
	{"kube log password param", `I1125 09:00:00.000000 1 db.go:42] connecting user=app password=hunter2-prod host=db`, []string{"hunter2-prod"}, []string{"user=app", "host=db"}},
	{"logfmt secret and key", `ts=1 caller=auth.go:9 secret=s3cr3tV4lue key=k-123456 level=warn`, []string{"s3cr3tV4lue", "k-123456"}, []string{"level=warn", "caller=auth.go:9"}},
	{"api_key in a query fragment", `GET /v1/items?page=2&api_key=AbCdEf123456&limit=50 200`, []string{"AbCdEf123456"}, []string{"page=2", "limit=50"}},
	{"apikey upper case", `retrying with APIKEY=ZXhhbXBsZWtleQ== after 401`, []string{"ZXhhbXBsZWtleQ=="}, []string{"after 401"}},
	{"access_token form body", `body: grant_type=refresh&access_token=ya29.a0AfH6SMBx&expires_in=3599`, []string{"ya29.a0AfH6SMBx"}, []string{"grant_type=refresh", "expires_in=3599"}},
	{"oauth code", `callback state=xyz&code=4/0AX4XfWh-secretcode`, []string{"4/0AX4XfWh-secretcode"}, []string{"state=xyz"}},
	{"sig in a form", `sv=2022-11-02&ss=b&sig=Zm9vYmFyYmF6%3D&se=2026`, []string{"Zm9vYmFyYmF6%3D"}, []string{"sv=2022-11-02", "se=2026"}},
	{"signature param", `x-goog-algorithm=GOOG4&Signature=0a1b2c3d4e5f`, []string{"0a1b2c3d4e5f"}, []string{"x-goog-algorithm=GOOG4"}},
	{"hyphenated header param", `x-api-key=k_live_51Hxyz upstream=svc`, []string{"k_live_51Hxyz"}, []string{"upstream=svc"}},
	{"env dump", "PATH=/usr/bin\nHOME=/root\nTOKEN=" + testGitHubToken + "\nSECRET=topsecretvalue\nDEBUG=1", []string{testGitHubToken, "topsecretvalue"}, []string{"PATH=/usr/bin", "HOME=/root", "DEBUG=1"}},
	{"env dump with export", `export PASSWORD=Sup3rS3cret! KEY=deadbeefcafe`, []string{"Sup3rS3cret!", "deadbeefcafe"}, []string{"export "}},
	{"postgres url", `dial failed: postgres://app:pgpass123@db.internal:5432/versus?sslmode=disable`, []string{"pgpass123", "sslmode=disable"}, []string{"postgres://db.internal:5432"}},
	{"dsn key value", `host=db port=5432 user=app password=dsnPass99 dbname=versus sslmode=require`, []string{"dsnPass99"}, []string{"dbname=versus", "sslmode=require"}},
	{"redis url", `redis: connection refused redis://:redispw@cache:6379/0`, []string{"redispw"}, []string{"redis://cache:6379"}},
	{"mongodb srv url", `mongodb+srv://u:mongopw@cluster0.example.net/test?retryWrites=true`, []string{"mongopw", "retryWrites"}, []string{"mongodb+srv://cluster0.example.net"}},
	{"jwt in a bearer url", `Post "https://api.example.com/hook?token=` + testJWT + `": EOF`, []string{testJWT}, []string{"https://api.example.com", "EOF"}},
	{"jwt token param", "ws upgrade token=" + testJWTExpiry + ";path=/", []string{testJWTExpiry + ";path=/"}, nil},
	{"password in json", `{"user":"app","password":"p@ss","note":"x"}`, nil, []string{`"user":"app"`, `"note":"x"`}},
	{"password=value in a json string", `{"dsn":"host=db password=jsonpw1 user=a"}`, []string{"jsonpw1"}, []string{"host=db", "user=a"}},
	{"pem header", testPEMBegin + "\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", nil, []string{testPEMBegin, "-----END RSA PRIVATE KEY-----"}},
	{"pem in an env value", `TLS_KEY=-----BEGIN key=pemkeyvalue`, []string{"pemkeyvalue"}, []string{"TLS_KEY="}},
	{"telegram bot path", `Post "https://api.telegram.org/bot123:AAHsecret/sendMessage": EOF`, nil, []string{"https://api.telegram.org"}},
	{"bot token in prose", `telegram 401 for bot987654:AAE-tok_en`, []string{"987654:AAE-tok_en"}, []string{"telegram 401 for bot"}},
	{"teams sig url", `Post "https://prod-1.westus.logic.azure.com:443/workflows/a/triggers/manual/paths/invoke?api-version=2016-06-01&sig=TEAMSSIG": 429`, []string{"TEAMSSIG"}, []string{"https://prod-1.westus.logic.azure.com:443", "429"}},
	{"slack webhook path", `slack: Post "https://hooks.slack.com/services/T000/B000/XXXXSLACK": timeout`, []string{"XXXXSLACK"}, []string{"https://hooks.slack.com"}},
	{"near miss names", `monkey=1 tokens=2 secretary=3 passwordless=true keyboard=us codec=h264`, nil, []string{"monkey=1", "tokens=2", "secretary=3", "passwordless=true", "keyboard=us", "codec=h264"}},
	{"name with a colon", `password: hunter2 token: abc`, nil, []string{"password: hunter2", "token: abc"}},
	{"empty values", `token= key=& secret="x"`, nil, []string{"token= ", "key=&", `secret="x"`}},
	{"multibyte before name", `ékey=multibytesecret 日本token=nihontoken`, []string{"multibytesecret", "nihontoken"}, nil},
	{"repeated value", `key=dup1 and later key=dup1`, []string{"dup1"}, nil},
	{"kube event message", `Warning  Failed  kubelet  Failed to pull image "registry.example.com/app:1": rpc error: code = Unknown desc = unauthorized`, nil, []string{"code = Unknown", `"registry.example.com/app:1"`}},
	{"aws key id in an sdk log", `level=debug msg="using static credentials" access_key=AKIAIOSFODNN7EXAMPLE source=env`, nil, []string{"source=env", `msg="using static credentials"`}},
	{"secret_key name", `boto config secret_key=wJalrXUtnFEMI/K7MDENG region=eu-west-1`, nil, []string{"region=eu-west-1"}},
	{"aws presigned url", `GET https://b.s3.amazonaws.com/o?X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20261125&X-Amz-Signature=abc 403`, []string{"AKIAIOSFODNN7EXAMPLE", "X-Amz-Signature=abc"}, []string{"https://b.s3.amazonaws.com", "403"}},
}

// Every secret the regex redactor removed is still removed by RedactSecrets,
// text both leave alone stays, and each line where the two outputs differ
// is logged for review.
func TestRedactSecretsKeepsRegexRedactions(t *testing.T) {
	for _, tc := range redactionParityCorpus {
		t.Run(tc.name, func(t *testing.T) {
			old, got := regexRedactSecrets(tc.in), RedactSecrets(tc.in)
			for _, secret := range tc.secrets {
				if strings.Contains(old, secret) {
					t.Fatalf("corpus: the regex redactor kept %q: %q", secret, old)
				}
				if strings.Contains(got, secret) {
					t.Errorf("scanner keeps %q that the regex redacted:\n  regex   %q\n  redactor %q", secret, old, got)
				}
			}
			for _, value := range regexRedactedValues(tc.in) {
				if strings.Contains(got, value) {
					t.Errorf("scanner keeps regex-redacted value %q: %q", value, got)
				}
			}
			for _, text := range tc.keep {
				if !strings.Contains(old, text) {
					t.Fatalf("corpus: the regex redactor cut %q: %q", text, old)
				}
				if !strings.Contains(got, text) {
					t.Errorf("scanner cuts %q that the regex kept:\n  regex   %q\n  redactor %q", text, old, got)
				}
			}
			if old != got {
				t.Logf("outputs differ:\n  regex   %q\n  redactor %q", old, got)
			}
		})
	}
}

// Over arbitrary text, a value the regex redactor removed entirely never
// appears in RedactSecrets' output.
func FuzzRedactSecretsKeepsRegexRedactions(f *testing.F) {
	for _, tc := range redactionParityCorpus {
		f.Add(tc.in)
	}
	// A key ID in a URL host, and a label inside a URL that the first pass
	// cuts to its host: a second run may cut more there, never less.
	f.Add("http://AKIAIOSFODNN7EXAMPLE.s3.amazonaws.com/o")
	f.Add("http://secret_key=abc")
	f.Fuzz(func(t *testing.T, s string) {
		got := RedactSecrets(s)
		for _, value := range regexRedactedValues(s) {
			if strings.Contains(got, value) {
				t.Fatalf("scanner keeps regex-redacted value %q\n  in     %q\n  regex  %q\n  redactor %q", value, s, regexRedactSecrets(s), got)
			}
		}
		// Nothing the regex redactor replaced survives, even in part: the
		// output is the regex redactor's with more spans replaced, and
		// nothing else changed.
		if old := regexRedactSecrets(s); !refines(old, got) {
			t.Fatalf("output is not the regex redactor's with more spans replaced\n  in      %q\n  regex   %q\n  redactor %q", s, old, got)
		}
		// A second run may only cut more text, never bring any back: a
		// marker inside a URL host lets the first pass cut the whole host
		// ("a://token=0"), as RedactSecrets documents.
		if again := RedactSecrets(got); !refines(got, again) {
			t.Fatalf("a second run did more than cut text: %q -> %q", got, again)
		}
	})
}
