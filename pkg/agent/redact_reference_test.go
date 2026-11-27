package agent

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/redaction"
)

// The agent's Redactor delegates to pkg/redaction, which skips a rule when the
// line lacks a literal every match of that rule contains. The redactor below is
// the agent's own implementation from before the delegation, frozen here (its
// rules, order, replacements, error messages and map handling).
//
// pkg/redaction has since gained rules for secrets written as key/value pairs
// (quoted keys, longer key names, quoted values, other authorization schemes,
// private key blocks). The tests require byte-identical results from the two
// redactors on every input those added rules do not match. Where they do
// match, the output must be exactly what pkg/redaction gives, whose own tests
// compare it with a frozen copy of the added rules, and for the corpus exactly
// the output pinned here. The redactor is a security boundary: any other
// difference may let a secret through.

var referenceAgentDefaults = []struct{ name, pattern, repl string }{
	{"jwt", `eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`, ""},
	{"aws_key", `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, ""},
	{"openai_key", `\bsk-[A-Za-z0-9_\-]{20,}`, ""},
	{"slack_token", `\bxox[abcdeprs]-[A-Za-z0-9\-]{10,}`, ""},
	{"basic_auth", `(://)[^/\s:@]+:[^/\s@]+(@)`, `${1}<REDACTED:basic_auth>${2}`},
	{"bearer", `(?i)Authorization:\s*Bearer\s+[A-Za-z0-9._\-]+`, ""},
	{"password", `(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key)\s*[=:]\s*\S+`, ""},
	{"email", `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, ""},
	{"uuid", `\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`, ""},
	{"user_agent", `Mozilla/[0-9.]+\s*\([^)]*\)(?:[^"\n]*?(?:Gecko|Chrome|Safari|Firefox|Edg|Trident|OPR|MSIE)[^"\n]*)?`, ""},
}

var referenceAgentIPs = []struct{ name, pattern string }{
	{"ipv4", `\b(?:\d{1,3}\.){3}\d{1,3}\b`},
	{"ipv6", `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`},
}

type referenceAgentRule struct {
	name string
	re   *regexp.Regexp
	repl string
}

type referenceAgentRedactor struct{ rules []referenceAgentRule }

func buildReferenceAgentRedactor(redactIPs bool, extra []string) (*referenceAgentRedactor, []error) {
	var errs []error
	r := &referenceAgentRedactor{}
	for _, d := range referenceAgentDefaults {
		r.rules = append(r.rules, referenceAgentRule{name: d.name, re: regexp.MustCompile(d.pattern), repl: d.repl})
	}
	if redactIPs {
		for _, d := range referenceAgentIPs {
			r.rules = append(r.rules, referenceAgentRule{name: d.name, re: regexp.MustCompile(d.pattern)})
		}
	}
	for i, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("extra redactor[%d] %q: %w", i, p, err))
			continue
		}
		r.rules = append(r.rules, referenceAgentRule{name: fmt.Sprintf("custom%d", i), re: re})
	}
	return r, errs
}

func newReferenceAgentRedactor(t testing.TB, redactIPs bool, extra []string) *referenceAgentRedactor {
	t.Helper()
	r, errs := buildReferenceAgentRedactor(redactIPs, extra)
	if len(errs) != 0 {
		t.Fatalf("reference redactor: %v", errs)
	}
	return r
}

func (r *referenceAgentRedactor) Scrub(s string) string {
	if r == nil || len(r.rules) == 0 || s == "" {
		return s
	}
	for _, rule := range r.rules {
		repl := rule.repl
		if repl == "" {
			repl = "<REDACTED:" + rule.name + ">"
		}
		s = rule.re.ReplaceAllString(s, repl)
	}
	return s
}

func (r *referenceAgentRedactor) ScrubFields(fields map[string]interface{}) map[string]interface{} {
	if r == nil || fields == nil {
		return fields
	}
	out := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		out[k] = r.scrubValue(v)
	}
	return out
}

func (r *referenceAgentRedactor) scrubValue(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return r.Scrub(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, item := range t {
			out[i] = r.scrubValue(item)
		}
		return out
	case map[string]interface{}:
		return r.ScrubFields(t)
	default:
		return v
	}
}

// The expressions of the rules added since the frozen redactor, as written,
// except that the blanks before a value on the key's line take a carriage
// return or form feed as well, as the scanner that replaced the secret
// expression skips them. The bearer rest rule runs on what the bearer rule
// leaves. The basic auth
// password rule is looked for in the value itself: the frozen redactor's
// e-mail rule can take a password and host (://:pw@host.tld) before it.
var (
	addedPrivateKeyPattern = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----|$)`)
	addedSecretPattern     = regexp.MustCompile(`(?is)(?P<keepBearer>(authorization:\s*bearer\s+)[a-z0-9._\-]+)|((?:authorization|cookie)\\*["']?\s*[:=]+>?)(?:(\s*\\+")((?:bearer|basic|token)[ \t]+)?(?:[^"\\]|\\[^"\\]|\\\\(?:\\\\|\\?.))*|(\s*""")((?:bearer|basic|token)[ \t]+)?(?:[^"]|"[^"]|""[^"])*|(\s*''')((?:bearer|basic|token)[ \t]+)?(?:[^']|'[^']|''[^'])*|(\s*")((?:bearer|basic|token)[ \t]+)?(?:[^"\\]|\\.)*(?:"[^\s,\]})"';:/<>][^\s]*)?|(\s*')((?:bearer|basic|token)[ \t]+)?(?:[^'\\]|\\.|'')*(?:'[^\s,\]})"';:/<>][^\s]*)?|(\s*\[)((?:bearer|basic|token)[ \t]+)?(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*'|\[(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*'|\[(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*')*\])*\])*(?:[\["'][^\n]*)?(?:\][^\s,\]})"';][^\s]*)?|(?P<linesAuthorization>[ \t\r\f]*(?:[|>][+\-0-9]*[ \t]*)?(?:#[^\n]*)?\r?\n)|(?P<header>[ \t\r\f]*)((?:bearer|basic|token)[ \t]+)?[^\n]+)|((?:passw(?:or)?d|pwd|passphrase|secret|token|credentials?|(?:api|access|secret|private|signing|encryption|ssh)[_\-]?key|secret_key_base|secret-key-base|secretkeybase|secret[_\-]?string)\\*["']?\s*[:=]+>?)(?:(\s*\\+")(?:[^"\\]|\\[^"\\]|\\\\(?:\\\\|\\?.))*|(\s*""")(?:[^"]|"[^"]|""[^"])*|(\s*''')(?:[^']|'[^']|''[^'])*|(\s*")(?:[^"\\]|\\.)*(?:"[^\s,\]})"';:/<>][^\s]*)?|(\s*')(?:[^'\\]|\\.|'')*(?:'[^\s,\]})"';:/<>][^\s]*)?|(\s*\[)(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*'|\[(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*'|\[(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*')*\])*\])*(?:[\["'][^\n]*)?(?:\][^\s,\]})"';][^\s]*)?|([ \t\r\f]*)(?P<keepLiteral>(?:true|false|null)(?:[\s,\]})]|$))|(?P<lines>[ \t\r\f]*(?:[|>][+\-0-9]*[ \t]*)?(?:#[^\n]*)?\r?\n)|([ \t\r\f]*)(?P<keepObject>\{[ \t]*\r?\n)|([ \t\r\f]*)\{[a-z0-9_\-]+\}\S+|([ \t\r\f]*)\{[^\n]*|([ \t\r\f]*)\S+)|((?:name|key)\\*["']?\s*[:=]+>?\s*\\*["']?[a-z0-9_.\-]*(?:passw(?:or)?d|pwd|passphrase|secret|token|credentials?|(?:api|access|secret|private|signing|encryption|ssh)[_\-]?key|secret_key_base|secret-key-base|secretkeybase|secret[_\-]?string|authorization|cookie)\\*["']?\s*,?\s*\\*["']?value\\*["']?\s*[:=]+>?)(?:(\s*\\+")(?:[^"\\]|\\[^"\\]|\\\\(?:\\\\|\\?.))*|(\s*""")(?:[^"]|"[^"]|""[^"])*|(\s*''')(?:[^']|'[^']|''[^'])*|(\s*")(?:[^"\\]|\\.)*(?:"[^\s,\]})"';:/<>][^\s]*)?|(\s*')(?:[^'\\]|\\.|'')*(?:'[^\s,\]})"';:/<>][^\s]*)?|(\s*\[)(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*'|\[(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*'|\[(?:[^\[\]"']|"(?:[^"\\]|\\.)*"|'[^']*')*\])*\])*(?:[\["'][^\n]*)?(?:\][^\s,\]})"';][^\s]*)?|([ \t\r\f]*)(?P<keepLiteralNamed>(?:true|false|null)(?:[\s,\]})]|$))|(?P<linesNamed>[ \t\r\f]*(?:[|>][+\-0-9]*[ \t]*)?(?:#[^\n]*)?\r?\n)|([ \t\r\f]*)(?P<keepObjectNamed>\{[ \t]*\r?\n)|([ \t\r\f]*)\{[a-z0-9_\-]+\}\S+|([ \t\r\f]*)\{[^\n]*|([ \t\r\f]*)\S+)`)
	addedBearerRestPattern = regexp.MustCompile(`(<REDACTED:bearer>)[~+/=%][A-Za-z0-9._~+/=%\-]*`)
	addedBasicAuthPassword = regexp.MustCompile(`(://:)[^/\s@]+(@)`)
)

// addedRuleMatches reports whether an added rule has something to match in
// value, or in frozen, the frozen redactor's output for it. The added rules
// run on text the earlier rules have already been through; those only put
// markers in place of values, which cannot form a key and its separator, so a
// pair the secret rule matches there is in value too.
func addedRuleMatches(value, frozen string) bool {
	return addedPrivateKeyPattern.MatchString(value) || addedSecretPattern.MatchString(value) ||
		addedBearerRestPattern.MatchString(frozen) || addedBasicAuthPassword.MatchString(value)
}

var shippedRedactorExtras = []string{`(?i)password=\S+`, `Authorization:\s*Bearer\s+\S+`}

// agentRedactionExamples covers every rule: matches, near-misses, overlaps,
// case variants and the non-ASCII runes (?i) folds onto k and s.
var agentRedactionExamples = []string{
	"token eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl-_x end", "eyJonly", "eyj.a.b",
	"key=AKIAIOSFODNN7EXAMPLE;", "ASIA" + "ABCDEFGHIJKLMNOP", "AKIAIOSFODNN7EXAMPL", "xAKIAIOSFODNN7EXAMPLE",
	"sk-" + "proj-abcdefghijklmnopqrstuvwxyz0123", "sk-short", "ask-" + "abcdefghijklmnopqrstuvw",
	"xoxb-" + "1234567890-abcdefghij", "xoxo-" + "1234567890abc", "axoxb-" + "1234567890abc",
	"https://user:p4ss@host/path", "https://user@host", "redis://:pw@cache", "s3://a/b:c@d",
	"Authorization: Bearer abc.def-ghi_jkl", "authorization:bearer x", "Authorization: Basic xyz", "Bearer abc",
	"password=hunter2", "PASSWD: x", "pwd=1", "Secret : abc", "token=abc", "api_key=abc", "API-KEY=zz",
	"tokens=abc", "mypassword=x", "password", "to\u212aen=abc", "api_\u212aey=1", "paſſword=x", "ſecret: v", "api_Key=1",
	"alice@example.com", "a@b", "@example.com", "émile@example.com",
	"123e4567-e89b-12d3-a456-426614174000", "123E4567-E89B-12D3-A456-426614174000", "123e4567-e89b-12d3-a456-42661417400",
	`ua="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36" n`,
	"Mozilla/4.0 (compatible; MSIE 6.0)", "mozilla/5.0 (X11) Gecko",
	"client 10.0.0.1 connected", "1.2.3", "fe80:0:0:1:2:3", "2001:db8:0:0:0:0:2:1",
	`level=error user=alice@example.com password=hunter2 ip=10.1.2.3 req=123e4567-e89b-12d3-a456-426614174000`,
	`{"headers":{"Authorization":"Bearer eyJ` + `hbGciOi.eyJzdWIi.sig"},"url":"https://bob:s3cr3t@api.example.com/v1"}`,
	`https://user:token=abc@host.example.com/password=1`,
	"\xff\xfepassword=x\xff", "toK\xe2\x84en=abc", "日本語 AKIAIOSFODNN7EXAMPLE", "", " ", "\n",
	// secrets written as key/value pairs, and keys that only look like them
	`{"password":"hunter2"}`, `{"token": "abc123"}`, `"Authorization":"Bearer opaque-abc123"`, `aws_secret_access_key=AKIAIOSFODNN7EXAMPLE/secret`,
	`db_password="correct horse battery" user=bob`, `client_secret: 'it''s a secret'`, `{"msg":"{\"api_key\":\"k-123456\"}"}`,
	`Authorization: Basic dXNlcjpwYXNz`, `Authorization: Bearer abc+def/ghi== next`, `X-Auth-Token: abcdef123456`,
	"-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", `x_secret: db_password: hunter-two`,
	`"token_count": 5`, `max_tokens: 4096`, `tokens=12`, `password_changed_at: 2026-11-01T00:00:00Z`, `secret_name: db-creds`,
	`automountServiceAccountToken: false`, `"client_secret": null`, `the token was invalid`,
	// Authorization with other schemes, bare values with punctuation, values
	// on the lines after the key, nested arrays, triple quotes, name/value
	// pairs, and text that nests more pairs than are followed at once
	`Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0tZTVhT3g6dWkybHAyYXhUTm1zeWFrdw==`, `curl -H "Authorization: SSWS 00QCjAl4MlV-WPXM" https://x`,
	`Authorization: Negotiate YIIhunter2`, `Authorization: Digest username="bob", response="hunter2"`,
	`api_token: aB3}xY9,qq`, `CLIENT_SECRET=)x7Ykd`, `REDIS_PASSWORD: {x7Ykd`, `x_token=password=abc,def`,
	"db_password: |\n  hunter2line1\n  line2\nnext: x", "db_password:\n  - hunter2\nuser: bob", "{\n  \"db_password\":\n    \"hunter2\"\n}",
	"x_token=a-password\n=hunter2", `"credentials": ["a", ["hunter2"], "hunter3"]`, `db_password = """hunter two"""`,
	`{"name":"DB_PASSWORD","value":"hunter2"}`, "- name: DB_PASSWORD\n  value: hunter2",
	strings.Repeat("k_token=[", 20) + "]" + " x_token: \" db_password=\"hunter2 x\" user=bob",
}

var agentFoldReplacer = strings.NewReplacer("k", "K", "K", "K", "s", "ſ", "S", "ſ")

func agentRedactionCorpus() []string {
	var corpus []string
	for _, value := range agentRedactionExamples {
		corpus = append(corpus, value, strings.ToUpper(value), strings.ToLower(value), agentFoldReplacer.Replace(value))
	}
	filler := strings.Repeat("ordinary log text with numbers 12345 and words ", 350)
	for _, secret := range []string{"password=hunter2", "AKIAIOSFODNN7EXAMPLE", "alice@example.com", "Authorization: Bearer tok"} {
		corpus = append(corpus, secret+" "+filler, filler+" "+secret+" "+filler, filler+" "+secret)
	}
	for _, shape := range []string{"json", "logfmt", "stack"} {
		corpus = append(corpus, benchLines(shape, 2<<10)[:4]...)
		corpus = append(corpus, benchLines(shape, 16<<10)[:1]...)
	}
	return corpus
}

// agentChangedOutputs pins, for the shipped configuration (IP rules on, the
// two shipped user patterns), the exact output of every corpus value on
// which the redactor differs from the frozen one.
var agentChangedOutputs = [][2]string{
	{"Authorization: Basic xyz", "Authorization: Basic <REDACTED:secret>"},
	{"AUTHORIZATION: BASIC XYZ", "AUTHORIZATION: BASIC <REDACTED:secret>"},
	{"authorization: basic xyz", "authorization: basic <REDACTED:secret>"},
	{"Authorization: Baſic xyz", "Authorization: Baſic <REDACTED:secret>"},
	{"ſecret : abc", "ſecret : <REDACTED:secret>"},
	{"ſecret: v", "ſecret: <REDACTED:secret>"},
	{"{\"headers\":{\"Authorization\":\"Bearer eyJ" + "hbGciOi.eyJzdWIi.sig\"},\"url\":\"https://bob:s3cr3t@api.example.com/v1\"}", "{\"headers\":{\"Authorization\":\"Bearer <REDACTED:secret>\"},\"url\":\"https://<REDACTED:basic_auth>@api.example.com/v1\"}"},
	{"{\"HEADERS\":{\"AUTHORIZATION\":\"BEARER EYJHBGCIOI.EYJZDWII.SIG\"},\"URL\":\"HTTPS://BOB:S3CR3T@API.EXAMPLE.COM/V1\"}", "{\"HEADERS\":{\"AUTHORIZATION\":\"BEARER <REDACTED:secret>\"},\"URL\":\"HTTPS://<REDACTED:basic_auth>@API.EXAMPLE.COM/V1\"}"},
	{"{\"headers\":{\"authorization\":\"bearer eyjhbgcioi.eyjzdwii.sig\"},\"url\":\"https://bob:s3cr3t@api.example.com/v1\"}", "{\"headers\":{\"authorization\":\"bearer <REDACTED:secret>\"},\"url\":\"https://<REDACTED:basic_auth>@api.example.com/v1\"}"},
	{"{\"headerſ\":{\"Authorization\":\"Bearer eyJhbGciOi.eyJzdWIi.ſig\"},\"url\":\"httpſ://bob:ſ3cr3t@api.example.com/v1\"}", "{\"headerſ\":{\"Authorization\":\"Bearer <REDACTED:secret>\"},\"url\":\"httpſ://<REDACTED:basic_auth>@api.example.com/v1\"}"},
	{"{\"password\":\"hunter2\"}", "{\"password\":\"<REDACTED:secret>\"}"},
	{"{\"PASSWORD\":\"HUNTER2\"}", "{\"PASSWORD\":\"<REDACTED:secret>\"}"},
	{"{\"paſſword\":\"hunter2\"}", "{\"paſſword\":\"<REDACTED:secret>\"}"},
	{"{\"token\": \"abc123\"}", "{\"token\": \"<REDACTED:secret>\"}"},
	{"{\"TOKEN\": \"ABC123\"}", "{\"TOKEN\": \"<REDACTED:secret>\"}"},
	{"{\"toKen\": \"abc123\"}", "{\"toKen\": \"<REDACTED:secret>\"}"},
	{"\"Authorization\":\"Bearer opaque-abc123\"", "\"Authorization\":\"Bearer <REDACTED:secret>\""},
	{"\"AUTHORIZATION\":\"BEARER OPAQUE-ABC123\"", "\"AUTHORIZATION\":\"BEARER <REDACTED:secret>\""},
	{"\"authorization\":\"bearer opaque-abc123\"", "\"authorization\":\"bearer <REDACTED:secret>\""},
	{"aws_secret_access_key=AKIAIOSFODNN7EXAMPLE/secret", "aws_secret_access_key=<REDACTED:secret>"},
	{"AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE/SECRET", "AWS_SECRET_ACCESS_KEY=<REDACTED:secret>"},
	{"aws_secret_access_key=akiaiosfodnn7example/secret", "aws_secret_access_key=<REDACTED:secret>"},
	{"awſ_ſecret_acceſſ_Key=AKIAIOſFODNN7EXAMPLE/ſecret", "awſ_ſecret_acceſſ_Key=<REDACTED:secret>"},
	{"db_password=\"correct horse battery\" user=bob", "db_<REDACTED:custom0> user=bob"},
	{"DB_PASSWORD=\"CORRECT HORSE BATTERY\" USER=BOB", "DB_<REDACTED:custom0> USER=BOB"},
	{"db_paſſword=\"correct horſe battery\" uſer=bob", "db_<REDACTED:custom0> uſer=bob"},
	{"client_secret: 'it''s a secret'", "client_secret: '<REDACTED:secret>'"},
	{"CLIENT_SECRET: 'IT''S A SECRET'", "CLIENT_SECRET: '<REDACTED:secret>'"},
	{"client_ſecret: 'it''ſ a ſecret'", "client_<REDACTED:password>"},
	{"{\"msg\":\"{\\\"api_key\\\":\\\"k-123456\\\"}\"}", "{\"msg\":\"{\\\"api_key\\\":\\\"<REDACTED:secret>\\\"}\"}"},
	{"{\"MSG\":\"{\\\"API_KEY\\\":\\\"K-123456\\\"}\"}", "{\"MSG\":\"{\\\"API_KEY\\\":\\\"<REDACTED:secret>\\\"}\"}"},
	{"{\"mſg\":\"{\\\"api_Key\\\":\\\"K-123456\\\"}\"}", "{\"mſg\":\"{\\\"api_Key\\\":\\\"<REDACTED:secret>\\\"}\"}"},
	{"Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic <REDACTED:secret>"},
	{"AUTHORIZATION: BASIC DXNLCJPWYXNZ", "AUTHORIZATION: BASIC <REDACTED:secret>"},
	{"authorization: basic dxnlcjpwyxnz", "authorization: basic <REDACTED:secret>"},
	{"Authorization: Baſic dXNlcjpwYXNz", "Authorization: Baſic <REDACTED:secret>"},
	{"Authorization: Bearer abc+def/ghi== next", "<REDACTED:bearer> next"},
	{"AUTHORIZATION: BEARER ABC+DEF/GHI== NEXT", "<REDACTED:bearer> NEXT"},
	{"authorization: bearer abc+def/ghi== next", "<REDACTED:bearer> next"},
	{"-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "<REDACTED:private_key>"},
	{"-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEPAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "<REDACTED:private_key>"},
	{"x_secret: db_password: hunter-two", "x_secret: <REDACTED:secret> <REDACTED:secret>"},
	{"X_SECRET: DB_PASSWORD: HUNTER-TWO", "X_SECRET: <REDACTED:secret> <REDACTED:secret>"},
	{"x_ſecret: db_paſſword: hunter-two", "x_<REDACTED:password> <REDACTED:secret>"},
	{"redis://:pw@cache", "redis://:<REDACTED:basic_auth>@cache"},
	{"REDIS://:PW@CACHE", "REDIS://:<REDACTED:basic_auth>@CACHE"},
	{"rediſ://:pw@cache", "rediſ://:<REDACTED:basic_auth>@cache"},
	{"x_token=a-password\n=hunter2", "x_token=<REDACTED:secret>\n=<REDACTED:secret>"},
	{"X_TOKEN=A-PASSWORD\n=HUNTER2", "X_TOKEN=<REDACTED:secret>\n=<REDACTED:secret>"},
	{"x_toKen=a-paſſword\n=hunter2", "x_toKen=<REDACTED:secret>\n=<REDACTED:secret>"},
	{"Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0tZTVhT3g6dWkybHAyYXhUTm1zeWFrdw==", "Authorization: <REDACTED:secret>"},
	{"AUTHORIZATION: APIKEY VNVHQ2ZHY0JDZGJRUW0TZTVHT3G6DWKYBHAYYXHUTM1ZEWFRDW==", "AUTHORIZATION: <REDACTED:secret>"},
	{"authorization: apikey vnvhq2zhy0jdzgjruw0tztvht3g6dwkybhayyxhutm1zewfrdw==", "authorization: <REDACTED:secret>"},
	{"Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0tZTVhT3g6dWKybHAyYXhUTm1zeWFrdw==", "Authorization: <REDACTED:secret>"},
	{"curl -H \"Authorization: SSWS 00QCjAl4MlV-WPXM\" https://x", "curl -H \"Authorization: <REDACTED:secret>\" https://x"},
	{"CURL -H \"AUTHORIZATION: SSWS 00QCJAL4MLV-WPXM\" HTTPS://X", "CURL -H \"AUTHORIZATION: <REDACTED:secret>\" HTTPS://X"},
	{"curl -h \"authorization: ssws 00qcjal4mlv-wpxm\" https://x", "curl -h \"authorization: <REDACTED:secret>\" https://x"},
	{"curl -H \"Authorization: ſſWſ 00QCjAl4MlV-WPXM\" httpſ://x", "curl -H \"Authorization: <REDACTED:secret>\" httpſ://x"},
	{"Authorization: Negotiate YIIhunter2", "Authorization: <REDACTED:secret>"},
	{"AUTHORIZATION: NEGOTIATE YIIHUNTER2", "AUTHORIZATION: <REDACTED:secret>"},
	{"authorization: negotiate yiihunter2", "authorization: <REDACTED:secret>"},
	{"Authorization: Digest username=\"bob\", response=\"hunter2\"", "Authorization: <REDACTED:secret>"},
	{"AUTHORIZATION: DIGEST USERNAME=\"BOB\", RESPONSE=\"HUNTER2\"", "AUTHORIZATION: <REDACTED:secret>"},
	{"authorization: digest username=\"bob\", response=\"hunter2\"", "authorization: <REDACTED:secret>"},
	{"Authorization: Digeſt uſername=\"bob\", reſponſe=\"hunter2\"", "Authorization: <REDACTED:secret>"},
	{"api_token: aB3}xY9,qq", "api_token: <REDACTED:secret>"},
	{"API_TOKEN: AB3}XY9,QQ", "API_TOKEN: <REDACTED:secret>"},
	{"api_token: ab3}xy9,qq", "api_token: <REDACTED:secret>"},
	{"api_toKen: aB3}xY9,qq", "api_toKen: <REDACTED:secret>"},
	{"CLIENT_SECRET=)x7Ykd", "CLIENT_SECRET=<REDACTED:secret>"},
	{"CLIENT_SECRET=)X7YKD", "CLIENT_SECRET=<REDACTED:secret>"},
	{"client_secret=)x7ykd", "client_secret=<REDACTED:secret>"},
	{"REDIS_PASSWORD: {x7Ykd", "REDIS_PASSWORD: <REDACTED:secret>"},
	{"REDIS_PASSWORD: {X7YKD", "REDIS_PASSWORD: <REDACTED:secret>"},
	{"redis_password: {x7ykd", "redis_password: <REDACTED:secret>"},
	{"REDIſ_PAſſWORD: {x7YKd", "REDIſ_PAſſWORD: <REDACTED:secret>"},
	{"x_token=password=abc,def", "x_token=<REDACTED:secret>"},
	{"X_TOKEN=PASSWORD=ABC,DEF", "X_TOKEN=<REDACTED:secret>"},
	{"x_toKen=paſſword=abc,def", "x_toKen=<REDACTED:secret>"},
	{"db_password: |\n  hunter2line1\n  line2\nnext: x", "db_password: |\n  <REDACTED:secret>\nnext: x"},
	{"DB_PASSWORD: |\n  HUNTER2LINE1\n  LINE2\nNEXT: X", "DB_PASSWORD: |\n  <REDACTED:secret>\nNEXT: X"},
	{"db_paſſword: |\n  hunter2line1\n  line2\nnext: x", "db_paſſword: |\n  <REDACTED:secret>\nnext: x"},
	{"db_password:\n  - hunter2\nuser: bob", "db_password:\n  - <REDACTED:secret>\nuser: bob"},
	{"DB_PASSWORD:\n  - HUNTER2\nUSER: BOB", "DB_PASSWORD:\n  - <REDACTED:secret>\nUSER: BOB"},
	{"db_paſſword:\n  - hunter2\nuſer: bob", "db_paſſword:\n  - <REDACTED:secret>\nuſer: bob"},
	{"{\n  \"db_password\":\n    \"hunter2\"\n}", "{\n  \"db_password\":\n    \"<REDACTED:secret>\"\n}"},
	{"{\n  \"DB_PASSWORD\":\n    \"HUNTER2\"\n}", "{\n  \"DB_PASSWORD\":\n    \"<REDACTED:secret>\"\n}"},
	{"{\n  \"db_paſſword\":\n    \"hunter2\"\n}", "{\n  \"db_paſſword\":\n    \"<REDACTED:secret>\"\n}"},
	{"\"credentials\": [\"a\", [\"hunter2\"], \"hunter3\"]", "\"credentials\": [<REDACTED:secret>]"},
	{"\"CREDENTIALS\": [\"A\", [\"HUNTER2\"], \"HUNTER3\"]", "\"CREDENTIALS\": [<REDACTED:secret>]"},
	{"\"credentialſ\": [\"a\", [\"hunter2\"], \"hunter3\"]", "\"credentialſ\": [<REDACTED:secret>]"},
	{"db_password = \"\"\"hunter two\"\"\"", "db_password = \"\"\"<REDACTED:secret>\"\"\""},
	{"DB_PASSWORD = \"\"\"HUNTER TWO\"\"\"", "DB_PASSWORD = \"\"\"<REDACTED:secret>\"\"\""},
	{"db_paſſword = \"\"\"hunter two\"\"\"", "db_paſſword = \"\"\"<REDACTED:secret>\"\"\""},
	{"{\"name\":\"DB_PASSWORD\",\"value\":\"hunter2\"}", "{\"name\":\"DB_PASSWORD\",\"value\":\"<REDACTED:secret>\"}"},
	{"{\"NAME\":\"DB_PASSWORD\",\"VALUE\":\"HUNTER2\"}", "{\"NAME\":\"DB_PASSWORD\",\"VALUE\":\"<REDACTED:secret>\"}"},
	{"{\"name\":\"db_password\",\"value\":\"hunter2\"}", "{\"name\":\"db_password\",\"value\":\"<REDACTED:secret>\"}"},
	{"{\"name\":\"DB_PAſſWORD\",\"value\":\"hunter2\"}", "{\"name\":\"DB_PAſſWORD\",\"value\":\"<REDACTED:secret>\"}"},
	{"- name: DB_PASSWORD\n  value: hunter2", "- name: DB_PASSWORD\n  value: <REDACTED:secret>"},
	{"- NAME: DB_PASSWORD\n  VALUE: HUNTER2", "- NAME: DB_PASSWORD\n  VALUE: <REDACTED:secret>"},
	{"- name: db_password\n  value: hunter2", "- name: db_password\n  value: <REDACTED:secret>"},
	{"- name: DB_PAſſWORD\n  value: hunter2", "- name: DB_PAſſWORD\n  value: <REDACTED:secret>"},
	{"k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[k_token=[] x_token: \" db_password=\"hunter2 x\" user=bob", "k_token=[<REDACTED:secret>"},
	{"K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[K_TOKEN=[] X_TOKEN: \" DB_PASSWORD=\"HUNTER2 X\" USER=BOB", "K_TOKEN=[<REDACTED:secret>"},
	{"K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[K_toKen=[] x_toKen: \" db_paſſword=\"hunter2 x\" uſer=bob", "K_toKen=[<REDACTED:secret>"},
}

func TestRedactorMatchesFrozenAgentRedactor(t *testing.T) {
	t.Parallel()
	corpus := agentRedactionCorpus()
	changedOutputs := map[string]string{}
	for _, pair := range agentChangedOutputs {
		changedOutputs[pair[0]] = pair[1]
	}
	for _, redactIPs := range []bool{false, true} {
		for _, extra := range [][]string{nil, shippedRedactorExtras} {
			redactIPs, extra := redactIPs, extra
			t.Run(fmt.Sprintf("ips=%t/extra=%d", redactIPs, len(extra)), func(t *testing.T) {
				t.Parallel()
				got, errs := NewRedactor(redactIPs, extra)
				if len(errs) != 0 {
					t.Fatalf("NewRedactor: %v", errs)
				}
				want := newReferenceAgentRedactor(t, redactIPs, extra)
				shared, _ := redaction.NewRedactor(redactIPs, extra)
				pinned := redactIPs && len(extra) > 0
				same, changed := 0, map[string]bool{}
				for _, value := range corpus {
					g, w := got.Scrub(value), want.Scrub(value)
					if g == w {
						same++
						continue
					}
					// The output differs from the frozen redactor: an added
					// rule must have something to match, the output must be
					// exactly what the shared redactor gives, and in the
					// shipped configuration exactly what is pinned below.
					changed[value] = true
					if !addedRuleMatches(value, w) {
						t.Errorf("Scrub(%.200q) differs from the frozen redactor though no added rule matches\n got  %.300q\n want %.300q", value, g, w)
					}
					if s := shared.Scrub(value); g != s {
						t.Errorf("Scrub(%.200q) differs from pkg/redaction\n got  %.300q\n want %.300q", value, g, s)
					}
					if !pinned {
						continue
					}
					if expected, ok := changedOutputs[value]; !ok {
						t.Errorf("Scrub(%.200q) = %.300q differs from the frozen redactor (%.300q) and has no pinned output", value, g, w)
					} else if g != expected {
						t.Errorf("Scrub(%.200q)\n got  %.300q\n want %.300q", value, g, expected)
					}
				}
				if same == 0 || len(changed) == 0 {
					t.Fatalf("%d values matched the frozen redactor and %d differed through an added rule; the corpus must have both", same, len(changed))
				}
				if pinned {
					for value := range changedOutputs {
						if !changed[value] {
							t.Errorf("pinned output for %.200q is not used: the value is not in the corpus or no longer differs from the frozen redactor", value)
						}
					}
				}
			})
		}
	}
}

// TestRedactorReplacesKeyValueSecrets covers the inputs the added rules
// match: through the agent's Redactor the secret is gone and the key, with
// its quoting, is still there. The frozen redactor let each of these through
// in whole or in part.
func TestRedactorReplacesKeyValueSecrets(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, secret, kept string }{
		{"json password", `{"user":"bob","password":"hunter2"}`, "hunter2", `"password":"<REDACTED:secret>"`},
		{"json token", `{"token": "abc123"}`, "abc123", `"token": "<REDACTED:secret>"`},
		{"json bearer", `"Authorization":"Bearer opaque-abc123"`, "opaque-abc123", `"Authorization":"Bearer <REDACTED:secret>"`},
		{"env aws secret", `aws_secret_access_key=wJalrXUtnFEMI/K7MDENG`, "wJalrXUtnFEMI", `aws_secret_access_key=<REDACTED:secret>`},
		{"logfmt quoted", `db_passwd="correct horse battery" user=bob`, "horse", `db_passwd="<REDACTED:secret>" user=bob`},
		{"yaml single quotes", "user: bob\nclient_secret: 'it''s a secret'", "a secret", `client_secret: '<REDACTED:secret>'`},
		{"json in a string", `{"msg":"{\"api_key\":\"k-123456\"}"}`, "k-123456", `\"api_key\":\"<REDACTED:secret>\"`},
		{"basic header", `Authorization: Basic dXNlcjpwYXNz`, "dXNlcjpwYXNz", `Authorization: Basic <REDACTED:secret>`},
		{"header", `x_auth_token: abcdef123456`, "abcdef123456", `x_auth_token: <REDACTED:secret>`},
		{"bearer header, base64 token", `Authorization: Bearer abc+def/ghi== next`, "def/ghi", `<REDACTED:bearer> next`},
		{"private key block", "key:\n-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEpAIBAAKCAQEA", "key:\n<REDACTED:private_key>"},
	}
	redactor, errs := NewRedactor(true, shippedRedactorExtras)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	frozen := newReferenceAgentRedactor(t, true, shippedRedactorExtras)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := frozen.Scrub(tc.in)
			if !strings.Contains(before, tc.secret) {
				t.Fatalf("the frozen redactor already removes %q from %q; the case does not show a difference", tc.secret, tc.in)
			}
			if !addedRuleMatches(tc.in, before) {
				t.Fatalf("no added rule matches %q", tc.in)
			}
			for form, got := range map[string]string{
				"Scrub":       redactor.Scrub(tc.in),
				"ScrubFields": redactor.ScrubFields(map[string]interface{}{"msg": tc.in})["msg"].(string),
			} {
				if strings.Contains(got, tc.secret) {
					t.Errorf("%s(%q) = %q still holds %q", form, tc.in, got, tc.secret)
				}
				if !strings.Contains(got, tc.kept) {
					t.Errorf("%s(%q) = %q, want it to keep %q", form, tc.in, got, tc.kept)
				}
			}
		})
	}
}

// TestRedactorFieldsUnderSecretKeys covers structured fields: the value of a
// field named like a secret is replaced whatever it looks like, at any depth,
// and fields that only look like secret keys are kept.
func TestRedactorFieldsUnderSecretKeys(t *testing.T) {
	t.Parallel()
	redactor, _ := NewRedactor(false, nil)
	got := redactor.ScrubFields(map[string]interface{}{
		"password":      "hunter2",
		"Authorization": "Bearer abc",
		"pin_token":     1234,
		"nested":        map[string]interface{}{"client_secret": "s-1", "token_count": 5, "has_token": true},
		"list":          []interface{}{map[string]interface{}{"api_key": "k-1"}},
		"secret_name":   "db-creds",
	})
	want := map[string]interface{}{
		"password":      "<REDACTED:secret>",
		"Authorization": "<REDACTED:secret>",
		"pin_token":     "<REDACTED:secret>",
		"nested":        map[string]interface{}{"client_secret": "<REDACTED:secret>", "token_count": 5, "has_token": true},
		"list":          []interface{}{map[string]interface{}{"api_key": "<REDACTED:secret>"}},
		"secret_name":   "db-creds",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScrubFields\n got  %#v\n want %#v", got, want)
	}
}

func TestRedactorFieldsMatchFrozenAgentRedactor(t *testing.T) {
	t.Parallel()
	fields := map[string]interface{}{
		"msg":    "login password=hunter2 from alice@example.com",
		"count":  3,
		"nil":    nil,
		"list":   []interface{}{"token=abc", 7, map[string]interface{}{"auth": "Authorization: Bearer x"}},
		"nested": map[string]interface{}{"url": "https://u:p@h/x", "empty": map[string]interface{}{}},
	}
	got, _ := NewRedactor(true, shippedRedactorExtras)
	want := newReferenceAgentRedactor(t, true, shippedRedactorExtras)
	if g, w := got.ScrubFields(fields), want.ScrubFields(fields); !reflect.DeepEqual(g, w) {
		t.Fatalf("ScrubFields\n got  %#v\n want %#v", g, w)
	}
	if got.ScrubFields(nil) != nil {
		t.Fatal("ScrubFields(nil) must return nil")
	}
	var unset *Redactor
	if unset.Scrub("password=x") != "password=x" || unset.ScrubFields(fields)["msg"] != fields["msg"] {
		t.Fatal("a nil Redactor must return its input unchanged")
	}
}

// TestRedactorInvalidExtrasMatchFrozenAgentRedactor keeps the error messages
// and the custom rule names (their position in the configured list) as they
// were, so a valid pattern after an invalid one still gets its own name.
func TestRedactorInvalidExtrasMatchFrozenAgentRedactor(t *testing.T) {
	t.Parallel()
	extra := []string{"((bad", `ticket-\d+`, "[also bad", `(?i)order-\d+`}
	got, gotErrs := NewRedactor(false, extra)
	want, wantErrs := buildReferenceAgentRedactor(false, extra)
	if fmt.Sprint(gotErrs) != fmt.Sprint(wantErrs) {
		t.Fatalf("errors\n got  %v\n want %v", gotErrs, wantErrs)
	}
	value := "ticket-12 ORDER-9 alice@example.com"
	if g, w := got.Scrub(value), want.Scrub(value); g != w {
		t.Fatalf("Scrub(%q) = %q, want %q", value, g, w)
	}
}

func FuzzAgentRedactorMatchesFrozenAgentRedactor(f *testing.F) {
	for _, value := range agentRedactionExamples {
		f.Add(value)
	}
	f.Add("access to\u212aen=abc") // U+212A KELVIN SIGN folds onto k under (?i)
	f.Add("0token:\r0")            // a carriage return between the separator and the value
	got, errs := NewRedactor(true, shippedRedactorExtras)
	if len(errs) != 0 {
		f.Fatalf("NewRedactor: %v", errs)
	}
	want := newReferenceAgentRedactor(f, true, shippedRedactorExtras)
	shared, _ := redaction.NewRedactor(true, shippedRedactorExtras)
	f.Fuzz(func(t *testing.T, value string) {
		g, w := got.Scrub(value), want.Scrub(value)
		if g == w {
			return
		}
		if !addedRuleMatches(value, w) {
			t.Fatalf("Scrub(%q) differs from the frozen redactor though no added rule matches\n got  %q\n want %q", value, g, w)
		}
		if s := shared.Scrub(value); g != s {
			t.Fatalf("Scrub(%q) differs from pkg/redaction\n got  %q\n want %q", value, g, s)
		}
	})
}
