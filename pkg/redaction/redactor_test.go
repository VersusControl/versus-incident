package redaction

import (
	"fmt"
	"math/rand"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode"
)

// The redactor skips a built-in rule when the value lacks a literal every match
// of that rule contains. The tests below keep a frozen redactor that runs
// every rule on every value: the reference, the rule list as it was before
// the shortcut and before the key/value rules (secret, private key block,
// rest of a bearer token, password-only URL).
//
// Without the added rules, the redactor must give byte-identical output to
// the reference on every input: a condition that misses a valid match lets a
// secret through. With them, whatever the reference removes from a value
// must be gone from the redactor's output too.

// referencePatterns is the rule list as it was before the shortcut and the
// key/value rules, frozen here so the comparison does not depend on the code
// under test.
var referencePatterns = []struct{ name, pattern, replacement string }{
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

// addedPatterns are the expression rules added to the reference list,
// frozen here as written. The secret rule has no expression: scanSecrets
// does its work.
var addedPatterns = []struct{ name, pattern, replacement string }{
	{"basic_auth_password", `(://:)[^/\s@]+(@)`, `${1}<REDACTED:basic_auth>${2}`},
	{"private_key", `-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----|$)`, ""},
	{"bearer_rest", `(<REDACTED:bearer>)[~+/=%][A-Za-z0-9._~+/=%\-]*`, `${1}`},
}

// addedRules names every rule added to the reference list.
var addedRules = map[string]bool{"basic_auth_password": true, "private_key": true, "secret": true, "bearer_rest": true}

// extendedOrder is the order the rules run in: the reference rules in their
// frozen order, with the added rules placed among them.
var extendedOrder = []string{
	"jwt", "aws_key", "openai_key", "slack_token", "basic_auth",
	"private_key", "secret",
	"bearer", "bearer_rest",
	"password", "basic_auth_password", "email", "uuid", "user_agent",
}

var referenceIPPatterns = []struct{ name, pattern string }{
	{"ipv4", `\b(?:\d{1,3}\.){3}\d{1,3}\b`},
	{"ipv6", `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`},
}

// shippedExtraPatterns are the user patterns in the shipped config.yaml.
var shippedExtraPatterns = []string{`(?i)password=\S+`, `Authorization:\s*Bearer\s+\S+`}

type referenceRule struct {
	name string
	re   *regexp.Regexp
	repl string
}

// referenceRedactor runs every rule on every value, in order.
type referenceRedactor struct{ rules []referenceRule }

func newReferenceRedactor(t testing.TB, redactIPs bool, extra []string) *referenceRedactor {
	t.Helper()
	reference := &referenceRedactor{}
	for _, rule := range referencePatterns {
		reference.rules = append(reference.rules, referenceRule{rule.name, regexp.MustCompile(rule.pattern), rule.replacement})
	}
	if redactIPs {
		for _, rule := range referenceIPPatterns {
			reference.rules = append(reference.rules, referenceRule{rule.name, regexp.MustCompile(rule.pattern), ""})
		}
	}
	for index, pattern := range extra {
		reference.rules = append(reference.rules, referenceRule{fmt.Sprintf("custom%d", index), regexp.MustCompile(pattern), ""})
	}
	return reference
}

func (reference *referenceRedactor) Scrub(value string) string {
	if value == "" {
		return value
	}
	for _, rule := range reference.rules {
		replacement := rule.repl
		if replacement == "" {
			replacement = "<REDACTED:" + rule.name + ">"
		}
		value = rule.re.ReplaceAllString(value, replacement)
	}
	return value
}

func newTestRedactor(t testing.TB, redactIPs bool, extra []string) *Redactor {
	t.Helper()
	redactor, errs := NewRedactor(redactIPs, extra)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	return redactor
}

// withoutRules returns a copy of redactor without the rules named in drop.
func withoutRules(redactor *Redactor, drop map[string]bool) *Redactor {
	copied := &Redactor{}
	for _, rule := range redactor.rules {
		if !drop[rule.name] {
			copied.rules = append(copied.rules, rule)
		}
	}
	return copied
}

// ruleReplaces reports whether rule changes value when it runs on its own.
func ruleReplaces(rule redactRule, value string) bool {
	if rule.scan {
		return scanSecrets(value, "<REDACTED:secret>", nil) != value
	}
	return rule.re.MatchString(value)
}

// referenceRemovals lists what the reference takes out of value: every
// match of each of its rules in the text that rule runs on, and the part of
// the match after its key and separator, where it has one. Only matches
// that are long and unusual enough to tell a leak from the same text
// standing elsewhere in value are listed.
func (reference *referenceRedactor) removals(value string) []string {
	var removed []string
	text := value
	for _, rule := range reference.rules {
		for _, match := range rule.re.FindAllString(text, -1) {
			candidates := []string{match}
			if cut := strings.IndexAny(match, ":= \t"); cut >= 0 {
				candidates = append(candidates, strings.TrimLeft(match[cut:], ":= \t\n\r\f"))
			}
			for _, candidate := range candidates {
				if distinctive(candidate, value) {
					removed = append(removed, candidate)
				}
			}
		}
		replacement := rule.repl
		if replacement == "" {
			replacement = "<REDACTED:" + rule.name + ">"
		}
		text = rule.re.ReplaceAllString(text, replacement)
	}
	return removed
}

type redactorPair struct {
	name      string
	redactor  *Redactor
	reference *referenceRedactor
}

func redactorPairs(t testing.TB) []redactorPair {
	t.Helper()
	var pairs []redactorPair
	for _, redactIPs := range []bool{false, true} {
		for _, extra := range [][]string{nil, shippedExtraPatterns} {
			pairs = append(pairs, redactorPair{
				name:      fmt.Sprintf("ips=%t/extra=%d", redactIPs, len(extra)),
				redactor:  newTestRedactor(t, redactIPs, extra),
				reference: newReferenceRedactor(t, redactIPs, extra),
			})
		}
	}
	return pairs
}

// ruleExamples holds, per rule, values the rule matches, near-misses it must
// not match, and values that only match because of case folding.
var ruleExamples = []string{
	// jwt
	"token eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl-_x end",
	"eyJ" + "a.b.c", "eyJonly", "eyj.a.b", "EYJA.B.C", "eyJ..x", "xeyJ" + "abc.def.ghi",
	// aws_key
	"key=AKIAIOSFODNN7EXAMPLE;", "ASIA" + "ABCDEFGHIJKLMNOP", "AKIAIOSFODNN7EXAMPL", "AKIAIOSFODNN7EXAMPLEX",
	"akiaIOSFODNN7EXAMPLE", "xAKIAIOSFODNN7EXAMPLE", "AKIA_IOSFODNN7EXAMPLE", "(ASIA" + "0123456789ABCDEF)",
	// openai_key
	"sk-" + "proj-abcdefghijklmnopqrstuvwxyz0123", "sk-short", "SK-abcdefghijklmnopqrstuvw", "ask-" + "abcdefghijklmnopqrstuvw",
	"-sk-" + "abcdefghijklmnopqrstuvwxyz", "sk-" + "_-_-_-_-_-_-_-_-_-_",
	// slack_token
	"xoxb-" + "1234567890-abcdefghij", "xoxp-" + "0000000000", "xoxo-" + "1234567890abc", "XOXB-1234567890abc", "xox-1234567890abc",
	"xoxs-" + "123456789", "axoxb-" + "1234567890abc",
	"xoxa-" + "1234567890abc", "xoxb-" + "1234567890abc", "xoxc-1234567890abc", "xoxd-1234567890abc",
	"xoxe-1234567890abc", "xoxp-" + "1234567890abc", "xoxr-" + "1234567890abc", "xoxs-" + "1234567890abc",
	// basic_auth
	"https://user:p4ss@host/path", "https://user@host", "postgres://u:p@db:5432/x", "://a:b@", "redis://:pw@cache",
	"ftp://us er:pw@host", "s3://a/b:c@d", "://a:b:c@d",
	// bearer
	"Authorization: Bearer abc.def-ghi_jkl", "authorization:bearer x", "AUTHORIZATION:   BEARER tok", "Authorization: Basic xyz",
	"Bearer abc", "Authorization:\tBearer\n tok", "Authorization : Bearer tok", "AuthoriZation:BeArEr t.k",
	// password
	"password=hunter2", "PASSWD: x", "pwd=1", "Secret : abc", "token=abc", "api_key=abc", "apikey: z", "API-KEY=zz",
	"tokens=abc", "mypassword=x", "password", "password = ", "password=\tvalue", "Token:\"quoted\"", "x_secret=1",
	"api key=1", "pwd:", "PaSsWoRd:x",
	// email
	"alice@example.com", "a@b", "x@y.c", "first.last+tag@sub.example.co.uk", "@example.com", "user@localhost",
	// uuid
	"123e4567-e89b-12d3-a456-426614174000", "123E4567-E89B-12D3-A456-426614174000", "123e4567-e89b-12d3-a456-42661417400",
	"123e4567-e89b-12d3-a456-4266141740000", "g23e4567-e89b-12d3-a456-426614174000", "123e4567e89b12d3a456426614174000",
	// user_agent
	`ua="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36" next`,
	"Mozilla/5.0 (X11)", "mozilla/5.0 (X11) Gecko", "Mozilla/ (x)", "Mozilla/4.0 (compatible; MSIE 6.0)", "Mozilla/.5 (x)", "Mozilla/5.0 (unterminated",
	// ipv4 / ipv6
	"client 10.0.0.1 connected", "999.999.999.999", "1.2.3", "1.2.3.4.5", "fe80::1", "fe80:0:0:1:2:3",
	"2001:db8:0:0:0:0:2:1", "a:b", "12:34:56",
	// bearer_rest
	"Authorization: Bearer abc+def/ghi== next", "<REDACTED:bearer>/tail", "<REDACTED:bearer>%2Fx", "<REDACTED:bearer> +x",
	// private_key
	"-----BEGIN " + "PRIVATE KEY-----", "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\nafter",
	"-----BEGIN " + "PGP PRIVATE KEY BLOCK-----\nlQOYBF\n-----END PGP PRIVATE KEY BLOCK-----", "-----BEGIN CERTIFICATE-----", "-----BEGIN PUBLIC KEY-----",
}

// keyValueLines are secrets written as key/value pairs in JSON, YAML, logfmt,
// env and header styles, next to keys that only look like secret keys.
var keyValueLines = []string{
	`{"password":"hunter2"}`, `{"token": "abc123"}`, `"Authorization":"Bearer eyJhbGciOi"`, `aws_secret_access_key=AKIAIOSFODNN7EXAMPLE/secret`,
	`{"user":"bob","db_password":"correct horse","nested":{"client_secret":"s3cr3t-value","list":[{"api_key":"k-123456"},{"apiKey":"k-654321"}]}}`,
	`{"msg":"{\"password\":\"hun\\\"ter2\",\"user\":\"bob\"}"}`, `{"msg":"login {\\\"token\\\":\\\"deep-nested\\\"}"}`,
	`password: "correct horse battery"`, `db_password: 'it''s a secret'`, `passphrase: 'open \' sesame'`, "private_key: |\n  next line",
	`X-Api-Key: abc123456`, `x-auth-token = abcdef123456`, `Proxy-Authorization: Basic dXNlcjpwYXNz`, `Authorization: token ghp_` + `abcdefghijklmnop`,
	`authorization="Basic dXNlcjpwYXNz"`, `'Authorization' => 'Bearer abc+def/ghi=='`, `{\"Authorization\":\"Bearer opaque-token\"}`,
	`SECRET_KEY=django-insecure-abcdef`, `export AWS_SECRET_ACCESS_KEY='wJalrXUtnFEMI/K7MDENG'`, `access-key: AKIDEXAMPLE1234`, `privateKey := "abc def"`,
	`credentials: [alpha-one, "beta two"] after`, `"credential": "cred-value-1"`, `userPassword: {SSHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=`,
	`client_secret=\raw-value next`, `api_key: =leading-separator`, `access_token==double-separator`, `pwd=>"arrow value"`,
	`msg="bad token: " db_password="hunter2 x" user=bob`, `x_secret: db_password: hunter-two`, `{"msg": "bad token: ", "api_key": "x y"}`,
	`x_token: Authorization: Basic abcdefgh`, `Authorization: db_password="CAN ARY"`, `Authorization: Bearer db_password: canary-value`,
	`client_secret="unterminated value`, `access_token=abc123&user=bob`, `login(password=hunter2222)`, `{password:hunter2222}`,
	`"token_count": 5`, `max_tokens: 4096`, `tokens=12`, `password_changed_at: 2026-11-01T00:00:00Z`, `secret_name: db-creds`, `secretName: db-creds`,
	`the token: was invalid`, `the token was invalid`, `automountServiceAccountToken: false`, `"client_secret": null, "x": 1`, `has_password=true`,
	`"credentials": {"user":"bob"}`, `"api_keys": ["a","b"]`, `"client_secret": "", "list": []`, `--token-auth-file=/etc/tokens.csv`,
	`secretKeyRef: {name: db, key: password}`, `authorization-mode=Node,RBAC`, `invalid credentials: user not found`,
	// other authorization schemes, cookies, punctuation in bare values,
	// values on the lines after the key, name/value pairs
	`Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0tZTVhT3g6dWkybHAyYXhUTm1zeWFrdw==`, `curl -H "Authorization: SSWS 00QCjAl4MlV-WPXM" https://x`,
	`Authorization: Negotiate YIIhunter2`, `Authorization: Digest username="bob", response="hunter2"`, `Cookie: session=hunter2; csrftoken=y`,
	`api_token: aB3}xY9,qq`, `CLIENT_SECRET=)x7Ykd`, `REDIS_PASSWORD: {x7Ykd`, `x_token=password=abc,def`,
	`https://h/login?access_token=abc&password=aB3,hunter2`, `ClientId=app;ClientSecret=abc;Password=aB3,hunter2;`,
	"db_password: |\n  hunter2line1\n  line2\nnext: x", "db_password:\n  - hunter2\nuser: bob", "{\n  \"db_password\":\n    \"hunter2\"\n}",
	"x_token=a-password\n=hunter2", `"credentials": ["a", ["hunter2"], "hunter3"]`, `db_password = """hunter two"""`,
	`{"name":"DB_PASSWORD","value":"hunter2"}`, "- name: DB_PASSWORD\n  value: hunter2", `{"db_password":"mysecret:2024"}`, `x_token: Xtoken: y`,
	`signing_key=hunter2`, `"SecretString":"hunter 2"`, `redis://:hunter2@host:6379/0`, `"token": {"value": "hunter2"}`,
	strings.Repeat("k_token=[", 20) + "]" + " x_token: \" db_password=\"hunter2 x\" user=bob",
	strings.Repeat("token:", 30) + `x x_secret: password: "hunter2 x"`,
	"pwd=pwd=\n0", "credentials:pwd=\nhunter2", strings.Repeat("token:", 30) + "x credentials:pwd=\nhunter2 next",
}

// mixedLines carry several secrets in one value, including overlaps where one
// rule's match sits inside another's.
var mixedLines = []string{
	`level=error msg="login failed" user=alice@example.com password=hunter2 ip=10.1.2.3 req=123e4567-e89b-12d3-a456-426614174000`,
	`{"headers":{"Authorization":"Bearer eyJ` + `hbGciOi.eyJzdWIi.sig"},"url":"https://bob:s3cr3t@api.example.com/v1"}`,
	`GET /x HTTP/1.1 Authorization: Bearer eyJ` + `a.eyJb.c User-Agent: Mozilla/5.0 (Macintosh) Gecko/20100101 Firefox/120.0`,
	`token=AKIAIOSFODNN7EXAMPLE secret: xoxb-` + `1234567890-abcdefghij api-key=sk-` + `abcdefghijklmnopqrstuvwxyz`,
	`dsn=postgres://svc:pw@db.internal:5432/app?sslmode=disable mail=ops@corp.example.org`,
	`https://user:token=abc@host.example.com/password=1`,
	`email://a:b@c.de x@y.zz`,
	`<REDACTED:jwt> password=<REDACTED:email>`,
	"multi\nline\npassword=a\nAuthorization: bearer b\n",
}

// unicodeLines mix non-ASCII text, the runes Unicode folds onto ASCII
// letters, and invalid UTF-8 with the rule triggers.
var unicodeLines = []string{
	"пароль password=секрет ✓",
	"токен to\u212aen=abc", // U+212A KELVIN SIGN
	"paſſword=x",           // U+017F LATIN SMALL LETTER LONG S
	"ſecret: value",
	"api_Key=abc",
	"pwd=пароль",
	"\xff\xfepassword=x\xff",
	"toK\xe2\x84en=abc",
	"to\xe2\x84\xaaen=abc",
	"user@exämple.com",
	"émile@example.com",
	"Authorization: Bearer 🔑token",
	"AUTHORIZATION: ʙEARER x",
	"日本語 AKIAIOSFODNN7EXAMPLE 日本語",
	"\x00password=\x00",
	"eyJé.a.b",
}

var foldReplacer = strings.NewReplacer("k", "K", "K", "K", "s", "ſ", "S", "ſ")

// caseVariants returns the value plus its upper-case, lower-case and
// non-ASCII-fold spellings, so every case-insensitive rule sees them.
func caseVariants(value string) []string {
	return []string{value, strings.ToUpper(value), strings.ToLower(value), foldReplacer.Replace(value)}
}

// longLines are long values with a secret at the start, middle and end.
func longLines() []string {
	filler := strings.Repeat("ordinary log text with numbers 12345 and words ", 350)
	var lines []string
	for _, secret := range []string{"password=hunter2", "AKIAIOSFODNN7EXAMPLE", "alice@example.com", "Authorization: Bearer tok"} {
		lines = append(lines, secret+" "+filler, filler+" "+secret+" "+filler, filler+" "+secret)
	}
	// Pairs nested thousands deep: each array runs to the end of the line, so
	// searching inside every value would read the line thousands of times.
	nested := strings.Repeat("x_token=[", 3000) + " api_secret=tail-value " + strings.Repeat(`x_token=" `, 3000)
	return append(lines, filler, nested, strings.Repeat("a", 1<<15), strings.Repeat("@", 2048), strings.Repeat("://", 1024))
}

func redactionCorpus() []string {
	var corpus []string
	for _, group := range [][]string{ruleExamples, mixedLines, unicodeLines, keyValueLines} {
		for _, value := range group {
			corpus = append(corpus, caseVariants(value)...)
		}
	}
	corpus = append(corpus, longLines()...)
	r := rand.New(rand.NewSource(1))
	for _, shape := range benchShapes {
		for _, size := range []int{2 << 10, 16 << 10} {
			corpus = append(corpus, shape(r, size))
		}
	}
	return append(corpus, "", " ", "\n")
}

// distinctive reports whether a replaced value is long and unusual enough
// that finding it in the output means it leaked, rather than that the same
// text also stands somewhere else in the input.
func distinctive(value, input string) bool {
	return len(value) >= 6 && !strings.Contains(value, "REDACTED") && strings.Count(input, value) == 1
}

// redactionMarker matches the text a rule puts in place of what it removes.
var redactionMarker = regexp.MustCompile(`<REDACTED:[A-Za-z0-9_]+>`)

// checkAgainstReference holds the redactor to the reference on one value:
// without the added rules the output is the reference's, and with them
// nothing the reference removes is left.
func checkAgainstReference(redactor *Redactor, reference *referenceRedactor, value string) error {
	if got, want := withoutRules(redactor, addedRules).Scrub(value), reference.Scrub(value); got != want {
		return fmt.Errorf("without the added rules Scrub(%.200q)\n got  %.300q\n want %.300q", value, got, want)
	}
	got := redactor.Scrub(value)
	// The marker text is not part of the value: a removed word such as
	// "secret" must not be found in <REDACTED:secret>.
	unmarked := redactionMarker.ReplaceAllString(got, "\x00")
	for _, removed := range reference.removals(value) {
		if strings.Contains(unmarked, removed) {
			return fmt.Errorf("Scrub(%.200q) = %.300q keeps %q, which the reference removes", value, got, removed)
		}
	}
	return nil
}

// TestRedactorMatchesReferenceOnCorpus is the differential test on the
// corpus: without the added rules the output equals the reference, with
// them nothing the reference removes is left, and the corpus holds values
// each added rule changes.
func TestRedactorMatchesReferenceOnCorpus(t *testing.T) {
	t.Parallel()
	corpus := redactionCorpus()
	for _, pair := range redactorPairs(t) {
		t.Run(pair.name, func(t *testing.T) {
			t.Parallel()
			changed := map[string]int{}
			for _, value := range corpus {
				if err := checkAgainstReference(pair.redactor, pair.reference, value); err != nil {
					t.Error(err)
				}
				for _, rule := range pair.redactor.rules {
					if addedRules[rule.name] && ruleReplaces(rule, value) {
						changed[rule.name]++
					}
				}
			}
			for name := range addedRules {
				if changed[name] == 0 {
					t.Errorf("corpus has no value the added %s rule changes", name)
				}
			}
		})
	}
}

// TestRedactorConditionHoldsWhereRuleMatches checks each condition on its own:
// wherever a built-in rule matches a value, the rule's condition must hold
// for that value. The end-to-end comparison alone could miss a bad condition
// when an earlier rule happens to remove the match first.
func TestRedactorConditionHoldsWhereRuleMatches(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, true, nil)
	matched := map[string]int{}
	for _, value := range redactionCorpus() {
		for _, rule := range redactor.rules {
			if ruleReplaces(rule, value) {
				matched[rule.name]++
				if !rule.need.holds(value) {
					t.Errorf("rule %s matches %.200q but its condition does not hold", rule.name, value)
				}
			}
		}
	}
	for _, rule := range redactor.rules {
		if matched[rule.name] == 0 {
			t.Errorf("corpus has no value the %s rule matches", rule.name)
		}
	}
}

// TestRedactorConditionHoldsOnValuesBuiltFromRuleExpressions builds values
// from each built-in rule's own expression, so a rule edited without its
// condition (a new keyword, a new prefix) fails here even when no
// hand-written example covers the edit. Every alternation branch and every
// optional part must yield at least one value the rule matches, and literals
// under (?i) are spelled with every rune in their Unicode case-fold orbit.
func TestRedactorConditionHoldsOnValuesBuiltFromRuleExpressions(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, true, nil)
	for _, rule := range redactor.rules {
		if rule.scan {
			continue // no expression; see the key/value tests
		}
		parsed, err := syntax.Parse(rule.re.String(), syntax.Perl)
		if err != nil {
			t.Fatalf("rule %s: %v", rule.name, err)
		}
		builder := &expressionValues{rand: rand.New(rand.NewSource(int64(len(rule.name)))), covered: map[branch]bool{}}
		builder.collectBranches(parsed)
		// At least 400 values per rule, then more until every branch has been
		// used by a value the rule matches. A rule with many nested choices
		// needs more values to reach each one.
		for sample := 0; sample < 400 || (sample < 40000 && len(builder.covered) < len(builder.branches)); sample++ {
			builder.choices = map[branch]bool{}
			var value strings.Builder
			builder.write(&value, parsed)
			for _, candidate := range []string{value.String(), "log " + value.String() + " end"} {
				if !rule.re.MatchString(candidate) {
					continue
				}
				for choice := range builder.choices {
					builder.covered[choice] = true
				}
				if !rule.need.holds(candidate) {
					t.Errorf("rule %s matches %q but its condition does not hold", rule.name, candidate)
				}
			}
		}
		for _, missed := range builder.branches {
			if !builder.covered[missed] {
				t.Errorf("rule %s: no matching value used branch %d of %s", rule.name, missed.index, missed.re)
			}
		}
	}
}

// expressionValues writes random values that follow a parsed expression and
// records which alternation branches and optional parts each value used.
type expressionValues struct {
	rand     *rand.Rand
	branches []branch
	choices  map[branch]bool
	covered  map[branch]bool
}

// branch is one alternative of an alternation, or the taken side of an
// optional part.
type branch struct {
	re    *syntax.Regexp
	index int
}

func (values *expressionValues) collectBranches(re *syntax.Regexp) {
	switch re.Op {
	case syntax.OpAlternate:
		for index := range re.Sub {
			values.branches = append(values.branches, branch{re, index})
		}
	case syntax.OpQuest:
		values.branches = append(values.branches, branch{re, 0})
	}
	for _, sub := range re.Sub {
		values.collectBranches(sub)
	}
}

func (values *expressionValues) write(out *strings.Builder, re *syntax.Regexp) {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 {
				orbit := []rune{r}
				for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
					orbit = append(orbit, folded)
				}
				r = orbit[values.rand.Intn(len(orbit))]
			}
			out.WriteRune(r)
		}
	case syntax.OpCharClass:
		pair := values.rand.Intn(len(re.Rune) / 2)
		low, high := re.Rune[2*pair], re.Rune[2*pair+1]
		if high-low > 300 {
			high = low + 300
		}
		out.WriteRune(low + rune(values.rand.Intn(int(high-low)+1)))
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		out.WriteByte(byte('a' + values.rand.Intn(26)))
	case syntax.OpCapture:
		values.write(out, re.Sub[0])
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			values.write(out, sub)
		}
	case syntax.OpAlternate:
		index := values.rand.Intn(len(re.Sub))
		values.choices[branch{re, index}] = true
		values.write(out, re.Sub[index])
	case syntax.OpQuest:
		if values.rand.Intn(2) == 0 {
			values.choices[branch{re, 0}] = true
			values.write(out, re.Sub[0])
		}
	case syntax.OpStar, syntax.OpPlus, syntax.OpRepeat:
		low, high := 0, 3
		switch re.Op {
		case syntax.OpPlus:
			low, high = 1, 4
		case syntax.OpRepeat:
			low, high = re.Min, re.Max
			if high < 0 {
				high = low + 3
			}
		}
		for range low + values.rand.Intn(high-low+1) {
			values.write(out, re.Sub[0])
		}
	}
}

func TestRedactorSkipsRuleWhenConditionIsAbsent(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, true, nil)
	for _, rule := range redactor.rules {
		if rule.need == nil {
			t.Errorf("built-in rule %s has no condition", rule.name)
			continue
		}
		if rule.need.holds("plain words 42") {
			t.Errorf("rule %s condition holds on a value with none of its literals", rule.name)
		}
	}
}

func TestRedactorUserPatternsAlwaysRun(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, false, []string{`(?i)internal-\d+`})
	got := redactor.Scrub("ticket INTERNAL-42 opened")
	if want := "ticket <REDACTED:custom0> opened"; got != want {
		t.Fatalf("Scrub = %q, want %q", got, want)
	}
	if custom := redactor.rules[len(redactor.rules)-1]; custom.need != nil {
		t.Fatalf("user pattern %s has a condition; user patterns must always run", custom.name)
	}
}

// TestRedactorKeepsRulesAndOrder pins the built-in rules to the frozen lists:
// every reference rule is still there as written and in the same relative
// order, and the added rules sit where extendedOrder puts them.
func TestRedactorKeepsRulesAndOrder(t *testing.T) {
	t.Parallel()
	if len(defaultRules) != len(extendedOrder) {
		t.Fatalf("%d built-in rules, want %d", len(defaultRules), len(extendedOrder))
	}
	var kept []string
	for index, rule := range defaultRules {
		if rule.name != extendedOrder[index] {
			t.Errorf("rule %d is %s, want %s", index, rule.name, extendedOrder[index])
		}
		for _, want := range referencePatterns {
			if rule.name == want.name {
				kept = append(kept, rule.name)
				if rule.pattern != want.pattern || rule.replacement != want.replacement || rule.scan {
					t.Errorf("rule %s = %q %q, want %q %q", rule.name, rule.pattern, rule.replacement, want.pattern, want.replacement)
				}
			}
		}
		for _, want := range addedPatterns {
			if rule.name == want.name && (rule.pattern != want.pattern || rule.replacement != want.replacement || rule.scan) {
				t.Errorf("rule %s = %q %q, want %q %q", rule.name, rule.pattern, rule.replacement, want.pattern, want.replacement)
			}
		}
		if rule.name == "secret" && (!rule.scan || rule.pattern != "" || rule.replacement != "<REDACTED:secret>") {
			t.Errorf("rule secret = %q %q scan=%t, want the key/value scanner", rule.pattern, rule.replacement, rule.scan)
		}
	}
	if len(kept) != len(referencePatterns) {
		t.Fatalf("built-in rules keep %v of the %d reference rules", kept, len(referencePatterns))
	}
	for index, want := range referencePatterns {
		if kept[index] != want.name {
			t.Errorf("reference rule %d is %s, want %s", index, kept[index], want.name)
		}
	}
	for index, rule := range ipRules {
		if want := referenceIPPatterns[index]; rule.name != want.name || rule.pattern != want.pattern {
			t.Errorf("ip rule %d = %s %q, want %s %q", index, rule.name, rule.pattern, want.name, want.pattern)
		}
	}
}

// TestFoldVariantsCoverUnicodeFolding ties the folded literal matching to the
// case folding the regexp package applies under (?i): for every ASCII letter
// the variants are exactly the runes in its Unicode simple-fold orbit.
func TestFoldVariantsCoverUnicodeFolding(t *testing.T) {
	t.Parallel()
	for letter := 'a'; letter <= 'z'; letter++ {
		orbit := map[string]bool{string(letter): true}
		for r := unicode.SimpleFold(letter); r != letter; r = unicode.SimpleFold(r) {
			orbit[string(r)] = true
		}
		variants := foldVariants(byte(letter))
		if len(variants) != len(orbit) {
			t.Errorf("fold variants of %c = %q, want the orbit %v", letter, variants, orbit)
			continue
		}
		for _, variant := range variants {
			if !orbit[variant] {
				t.Errorf("fold variant %q of %c is not in its orbit %v", variant, letter, orbit)
			}
		}
	}
}

func FuzzRedactorMatchesReference(f *testing.F) {
	for _, group := range [][]string{ruleExamples, mixedLines, unicodeLines, keyValueLines} {
		for _, value := range group {
			f.Add(value)
		}
	}
	// A key in the word another key's value reaches over, at a line end.
	f.Add("pwd:pwd:\npwd:\n000000")
	// A key after the pairs dropped at the limit, its value on the next line.
	f.Add(strings.Repeat("token:", 17) + "0 pwd=\n000000")
	// A removed word that is also the name in a marker.
	f.Add("seCret:secret :0")
	redactor := newTestRedactor(f, true, shippedExtraPatterns)
	reference := newReferenceRedactor(f, true, shippedExtraPatterns)
	f.Fuzz(func(t *testing.T, value string) {
		if err := checkAgainstReference(redactor, reference, value); err != nil {
			t.Fatal(err)
		}
		for _, rule := range redactor.rules {
			if ruleReplaces(rule, value) && !rule.need.holds(value) {
				t.Fatalf("rule %s matches %q but its condition does not hold", rule.name, value)
			}
		}
	})
}

// The benchmark corpus has the shapes a Kubernetes workload writes (JSON,
// logfmt, a stack trace joined into one line), 2 to 16 KiB long, full of
// high-cardinality identifiers. It is built in code.

var benchShapes = []func(r *rand.Rand, size int) string{benchJSONLine, benchLogfmtLine, benchStackLine}

func benchHex(r *rand.Rand, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[r.Intn(16)]
	}
	return string(b)
}

func benchUUID(r *rand.Rand) string {
	return benchHex(r, 8) + "-" + benchHex(r, 4) + "-" + benchHex(r, 4) + "-" + benchHex(r, 4) + "-" + benchHex(r, 12)
}

func benchTimestamp(r *rand.Rand) string {
	return fmt.Sprintf("2026-11-21T%02d:%02d:%02d.%03dZ", r.Intn(24), r.Intn(60), r.Intn(60), r.Intn(1000))
}

func benchJSONLine(r *rand.Rand, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"ts":"%s","level":"info","msg":"request completed","request_id":"%s","trace_id":"%s","pod":"checkout-%s","user_id":%d,"path":"/api/v1/orders/%d","client":"10.%d.%d.%d"`,
		benchTimestamp(r), benchUUID(r), benchHex(r, 32), benchHex(r, 9), r.Intn(10_000_000), r.Intn(1_000_000), r.Intn(50), r.Intn(256), r.Intn(256))
	b.WriteString(`,"attrs":{`)
	for i := 0; b.Len() < size-2; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, `"sku_%d":"%s"`, i, benchHex(r, 12))
		case 1:
			fmt.Fprintf(&b, `"qty_%d":%d`, i, r.Intn(1000))
		case 2:
			fmt.Fprintf(&b, `"ref_%d":"%s"`, i, benchUUID(r))
		default:
			fmt.Fprintf(&b, `"note_%d":"item reserved in warehouse %d zone %c"`, i, r.Intn(90), 'a'+rune(r.Intn(6)))
		}
	}
	b.WriteString("}}")
	return b.String()
}

func benchLogfmtLine(r *rand.Rand, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `ts=%s level=warn caller=handler/orders.go:%d msg="cache miss for key" request_id=%s trace_id=%s pod=payments-%s duration=%dms status=503`,
		benchTimestamp(r), 100+r.Intn(400), benchUUID(r), benchHex(r, 32), benchHex(r, 9), r.Intn(5000))
	for i := 0; b.Len() < size; i++ {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, " item%d=%s", i, benchHex(r, 12))
		case 1:
			fmt.Fprintf(&b, " count%d=%d", i, r.Intn(100000))
		case 2:
			fmt.Fprintf(&b, " ref%d=%s", i, benchUUID(r))
		default:
			fmt.Fprintf(&b, ` region%d="eu-west-%d zone %c"`, i, r.Intn(3), 'a'+rune(r.Intn(3)))
		}
	}
	return b.String()
}

func benchStackLine(r *rand.Rand, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s ERROR [orders-%s] c.e.orders.OrderService - failed to reserve order %d for request %s\n",
		benchTimestamp(r), benchHex(r, 9), r.Intn(10_000_000), benchUUID(r))
	fmt.Fprintf(&b, "java.lang.IllegalStateException: inventory row %d locked by transaction %s\n", r.Intn(1_000_000), benchHex(r, 16))
	for i := 0; b.Len() < size; i++ {
		if i > 0 && i%25 == 0 {
			fmt.Fprintf(&b, "Caused by: java.sql.SQLException: lock wait timeout exceeded; try restarting transaction (session %s)\n", benchHex(r, 8))
		}
		fmt.Fprintf(&b, "\tat com.example.orders.OrderService.method%d(OrderService.java:%d)\n", r.Intn(8), 20+r.Intn(900))
	}
	return b.String()
}

// benchMixedLines interleaves every shape at 2, 4, 8 and 16 KiB.
func benchMixedLines() []string {
	r := rand.New(rand.NewSource(7))
	var lines []string
	for _, shape := range benchShapes {
		for _, size := range []int{2 << 10, 4 << 10, 8 << 10, 16 << 10} {
			for range 64 {
				lines = append(lines, shape(r, size))
			}
		}
	}
	r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
	return lines
}

// BenchmarkRedactorMixedLines compares the redactor with the reference that
// runs every rule on every line (the rule list from before the conditions and
// the key/value rules), for the built-in rules alone and for the
// shipped config (built-in rules plus its two user patterns). ns/op is per
// line.
func BenchmarkRedactorMixedLines(b *testing.B) {
	lines := benchMixedLines()
	var total int64
	for _, line := range lines {
		total += int64(len(line))
	}
	for _, config := range []struct {
		name  string
		extra []string
	}{{"builtin", nil}, {"shipped", shippedExtraPatterns}} {
		scrubbers := []struct {
			name  string
			scrub func(string) string
		}{
			{"reference", newReferenceRedactor(b, false, config.extra).Scrub},
			{"conditional", newTestRedactor(b, false, config.extra).Scrub},
		}
		for _, scrubber := range scrubbers {
			b.Run(config.name+"/"+scrubber.name, func(b *testing.B) {
				b.SetBytes(total / int64(len(lines)))
				b.ReportAllocs()
				sink := 0
				for i := range b.N {
					sink += len(scrubber.scrub(lines[i%len(lines)]))
				}
				if sink == 0 {
					b.Fatal("benchmark produced no output")
				}
			})
		}
	}
}
