package redaction

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The tests in this file cover secrets written as key/value pairs. The older
// bearer and password rules replace a pair whole when the key stands alone
// (`password=x`, `Authorization: Bearer x`); the cases here mostly use the
// forms those rules miss, where the key and the quoting are kept.

func TestRedactorReplacesKeyValueSecrets(t *testing.T) {
	t.Parallel()
	const secret = "<REDACTED:secret>"
	const auth = secret
	cases := []struct{ name, in, want string }{
		// The four forms that used to pass through unchanged.
		{"json password", `{"password":"hunter2"}`, `{"password":"` + secret + `"}`},
		{"json token", `{"token": "abc123"}`, `{"token": "` + secret + `"}`},
		{"json bearer", `"Authorization":"Bearer opaque-abc123"`, `"Authorization":"Bearer ` + auth + `"`},
		{"env aws secret", `aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCY`, `aws_secret_access_key=` + secret},
		{"env aws secret after key id", `aws_secret_access_key=AKIAIOSFODNN7EXAMPLE/secret`, `aws_secret_access_key=` + secret},

		// Keys: every secret word, alone and with a prefix, any case.
		{"key passwd", `"passwd":"v-1"`, `"passwd":"` + secret + `"`},
		{"key pwd", `"pwd":"v-1"`, `"pwd":"` + secret + `"`},
		{"key passphrase", `key_passphrase="v 1"`, `key_passphrase="` + secret + `"`},
		{"key secret", `"secret":"v-1"`, `"secret":"` + secret + `"`},
		{"key client_secret", `client_secret=v-1 next`, `client_secret=` + secret + ` next`},
		{"key clientSecret", `clientSecret: v-1`, `clientSecret: ` + secret},
		{"key api_key", `"api_key":"v-1"`, `"api_key":"` + secret + `"`},
		{"key apikey", `"apikey":"v-1"`, `"apikey":"` + secret + `"`},
		{"key x-api-key", `"x-api-key":"v-1"`, `"x-api-key":"` + secret + `"`},
		{"key access_key", `s3_access_key=v-1`, `s3_access_key=` + secret},
		{"key secret_key", `DJANGO_SECRET_KEY=v-1`, `DJANGO_SECRET_KEY=` + secret},
		{"key private_key", `"private_key":"v-1"`, `"private_key":"` + secret + `"`},
		{"key credential", `"credential":"v-1"`, `"credential":"` + secret + `"`},
		{"key credentials", `db.credentials=v-1`, `db.credentials=` + secret},
		{"key access_token", `access_token=v-1&user=bob`, `access_token=` + secret},
		{"key upper", `DB_PASSWORD=v-1`, `DB_PASSWORD=` + secret},
		{"key with long s and kelvin sign", "pa\u017f\u017fphra\u017fe=v-1 acce\u017f\u017f_to\u212aen=v-2 xapi_\u212aey=v-3", "pa\u017f\u017fphra\u017fe=" + secret + " acce\u017f\u017f_to\u212aen=" + secret + " xapi_\u212aey=" + secret},

		// Separators.
		{"spaces around equals", `db_password = v-1`, `db_password = ` + secret},
		{"tab after colon", "db_password:\tv-1", "db_password:\t" + secret},
		{"arrow", `'db_password' => 'v 1'`, `'db_password' => '` + secret + `'`},
		{"walrus", `clientSecret := "v 1"`, `clientSecret := "` + secret + `"`},
		{"double equals", `access_token==v-1`, `access_token==` + secret},
		{"value starts with a separator", `api_token: =v-1`, `api_token: ` + secret},

		// Values.
		{"quoted with spaces", `db_password="correct horse battery" user=bob`, `db_password="` + secret + `" user=bob`},
		{"escaped quote", `clientSecret: "a\"b c" next`, `clientSecret: "` + secret + `" next`},
		{"escaped backslash at end", `clientSecret: "ab\\" next`, `clientSecret: "` + secret + `" next`},
		{"single quotes", `db_password: 'v 1' next`, `db_password: '` + secret + `' next`},
		{"single quotes doubled", `db_password: 'it''s v' next`, `db_password: '` + secret + `' next`},
		{"single quotes escaped", `db_password: 'it\'s v' next`, `db_password: '` + secret + `' next`},
		{"unterminated quote", `db_password="v 1 and the rest`, `db_password="` + secret},
		{"bare takes the rest of an unspaced flow map", `{db_password:v-1,user:bob}`, `{db_password:` + secret},
		{"bare takes the closing bracket", `login(db_password=v-1)`, `login(db_password=` + secret},
		{"bare number", `"pin_token": 123456}`, `"pin_token": ` + secret},
		{"bare with backslash", `client_secret=\v-1 next`, `client_secret=` + secret + ` next`},
		{"scheme prefix", `userPassword: {SSHA}W6ph5Mm5Pz8= next`, `userPassword: ` + secret + ` next`},
		{"array", `"credentials": ["a b", "c"], "n": 1`, `"credentials": [` + secret + `], "n": 1`},
		{"yaml block scalar", "db_password: |\n  hunter2line1\n  line2\nnext: x", "db_password: |\n  " + secret + "\nnext: x"},

		// JSON: nested objects, arrays of objects, JSON inside a string.
		{"nested json", `{"a":{"db_password":"v 1","b":{"client_secret":"v-2"}},"user":"bob"}`, `{"a":{"db_password":"` + secret + `","b":{"client_secret":"` + secret + `"}},"user":"bob"}`},
		{"object under a secret key", `"credentials": {"user":"bob","db_password":"v 1"}`, `"credentials": ` + secret},
		{"array of objects", `[{"api_token":"v-1"},{"api_token":"v-2"}]`, `[{"api_token":"` + secret + `"},{"api_token":"` + secret + `"}]`},
		{"json in a string", `{"msg":"{\"db_password\":\"v 1\",\"user\":\"bob\"}"}`, `{"msg":"{\"db_password\":\"` + secret + `\",\"user\":\"bob\"}"}`},
		{"json in a string, escaped quote in value", `{"msg":"{\"db_password\":\"hun\\\"ter 2\",\"user\":\"bob\"}"}`, `{"msg":"{\"db_password\":\"` + secret + `\",\"user\":\"bob\"}"}`},
		{"json in a string, number", `{"msg":"{\"pin_token\":1234,\"user\":\"bob\"}"}`, `{"msg":"{\"pin_token\":` + secret},
		{"json in a string in a string", `"{\\\"db_password\\\":\\\"v-1\\\"}"`, `"{\\\"db_password\\\":\\\"` + secret + `"`},

		// YAML.
		{"yaml", "user: bob\ndb_password: v-1\nport: 5432", "user: bob\ndb_password: " + secret + "\nport: 5432"},
		{"yaml nested map", "x_secret:\n  db_password: v-1", "x_secret:\n  db_password: " + secret},
		{"yaml nested map on one line", `x_secret: db_password: v-1`, `x_secret: ` + secret + ` ` + secret},
		{"yaml flow map", `x_secret: {db_password: v-1, user: bob}`, `x_secret: ` + secret},

		// Authorization, header and JSON forms, with and without a scheme.
		{"basic header", `Authorization: Basic dXNlcjpwYXNz`, `Authorization: Basic ` + auth},
		{"token header", `Authorization: token ghp_abcdef`, `Authorization: token ` + auth},
		{"no scheme", `Authorization: abcdef123`, `Authorization: ` + auth},
		{"proxy header", `Proxy-Authorization: Basic dXNlcjpwYXNz`, `Proxy-Authorization: Basic ` + auth},
		{"json basic", `{"authorization":"Basic dXNlcjpwYXNz","x":1}`, `{"authorization":"Basic ` + auth + `","x":1}`},
		{"json in a string", `{\"Authorization\":\"Bearer abc+def/ghi==\"}`, `{\"Authorization\":\"Bearer ` + auth + `\"}`},
		{"logfmt", `authorization="Bearer abc+def/ghi==" next=1`, `authorization="Bearer ` + auth + `" next=1`},
		{"curl", `curl -H 'Authorization: Basic dXNlcjpwYXNz' https://x`, `curl -H 'Authorization: Basic ` + auth + `' https://x`},
		{"go header map", `map[Accept:[*/*] Authorization:[Basic dXNlcjpwYXNz]]`, `map[Accept:[*/*] Authorization:[Basic ` + auth + `]]`},
		{"quoted with spaces", `authorization="Negotiate abc def" next=1`, `authorization="` + auth + `" next=1`},
		{"space before colon", `Authorization : Bearer abc.def`, `Authorization : Bearer ` + auth},
		{"bearer header keeps its marker", `Authorization: Bearer abc.def-ghi next`, `<REDACTED:bearer> next`},
		{"bearer header, base64 token", `Authorization: Bearer abc+def/ghi== next`, `<REDACTED:bearer> next`},
		{"bearer header, percent-encoded token", `Authorization: Bearer abc%2Fdef~x next`, `<REDACTED:bearer> next`},

		// PEM private keys.
		{"pem begin line", `-----BEGIN RSA PRIVATE KEY-----`, `<REDACTED:private_key>`},
		{"pem block", "key:\n-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\nkqhkiG9w0BAQEF\n-----END PRIVATE KEY-----\nafter", "key:\n<REDACTED:private_key>\nafter"},
		{"pem block without end", "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBase64", `<REDACTED:private_key>`},
		{"pgp block", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF\n-----END PGP PRIVATE KEY BLOCK----- x", `<REDACTED:private_key> x`},
		{"pem under a key", `"private_key": "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n", "client_email": "x"`, `"private_key": "` + secret + `", "client_email": "x"`},
		{"pem as a bare value", "private_key: -----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\nnext", "private_key: " + secret + "\nnext"},

		// A value that ends with another key.
		{"quote closes an earlier string", `msg="bad x_token: " db_password="hunter2 x" user=bob`, `msg="bad x_token: "` + secret + `" user=bob`},
		{"empty value before a quoted pair", `x_token= api_secret="a b"`, `x_token= ` + secret + `"`},
		{"key as a value", `x_token: Authorization: Basic abcdefgh`, `x_token: ` + secret + ` Basic ` + auth},
		{"value ending in a key word", `client_secret=mysecret next`, `client_secret=` + secret + ` next`},
		{"authorization before a pair", `Authorization: db_password="CAN ARY"`, `Authorization: ` + secret},
		{"empty value before a json pair", `x_token= "api_secret": "a b"`, `x_token= "` + secret + `": "` + secret + `"`},
		{"array with a bracket in a string", `"credentials": ["a]b", "c d"], "n": 1`, `"credentials": [` + secret + `], "n": 1`},
		{"bearer before a pair", `Authorization: Bearer db_password: canary`, `<REDACTED:bearer>: ` + secret},
		{"bearer header under a key", `x_token: Authorization: Bearer abc.def`, `x_token: ` + secret + ` Bearer ` + secret},

		// Authorization with a scheme the rule does not know: the whole
		// value goes, up to the end of the line or the closing quote.
		{"unknown scheme", `Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0tZTVhT3g6dWkybHAyYXhUTm1zeWFrdw==`, `Authorization: ` + secret},
		{"unknown scheme in a quoted header", `curl -H "Authorization: SSWS 00QCjAl4MlV-WPXM" https://x`, `curl -H "Authorization: ` + secret + `" https://x`},
		{"negotiate", `Authorization: Negotiate YIIhunter2`, `Authorization: ` + secret},
		{"digest", `Authorization: Digest username="bob", response="hunter2"`, `Authorization: ` + secret},
		{"digest in a quoted header", `-H "Authorization: Digest username=\"bob\", response=\"hunter2\"" https://x`, `-H "Authorization: ` + secret + `" https://x`},
		{"unknown scheme, line by line", "Authorization: SSWS abc def\nHost: example.com", "Authorization: " + secret + "\nHost: example.com"},
		{"known scheme, two words", `Authorization: Basic abc def`, `Authorization: Basic ` + secret},
		{"header list in yaml", "Authorization:\n- SSWS abc def\nHost:\n- example.com", "Authorization:\n- " + secret + "\nHost:\n- example.com"},
		{"header list in json", `[{"name":"Authorization","value":"SSWS abc def"},{"name":"Host","value":"example.com"}]`, `[{"name":"Authorization","value":"` + secret + `"},{"name":"Host","value":"example.com"}]`},

		// A bare value may hold any punctuation: only whitespace ends it.
		{"bare with closing brace and comma", `api_token: aB3}xY9,qq`, `api_token: ` + secret},
		{"bare starting with a bracket", `CLIENT_SECRET=)x7Ykd next`, `CLIENT_SECRET=` + secret + ` next`},
		{"bare starting with a brace", `REDIS_PASSWORD: {x7Ykd`, `REDIS_PASSWORD: ` + secret},
		{"pair as a bare value, with a comma", `x_token=password=abc,def`, `x_token=` + secret},

		// YAML values on the lines after the key, and JSON broken after the colon.
		{"block scalar, folded and stripped", "  db_password: >-\n    hunter two\n\n    line three\n  user: bob", "  db_password: >-\n    " + secret + "\n  user: bob"},
		{"block scalar with indent indicator and comment", "db_password: |2+ # note\n   hunter2\nuser: bob", "db_password: |2+ " + secret + "\nuser: bob"},
		{"sequence under a secret key", "db_password:\n  - hunter2\n  - hunter3\nuser: bob", "db_password:\n  - " + secret + "\nuser: bob"},
		{"sequence at the key's indent", "credentials:\n- hunter2\n- hunter3\nuser: bob", "credentials:\n- " + secret + "\nuser: bob"},
		{"plain scalar on the next line", "db_password:\n  hunter2\nuser: bob", "db_password:\n  " + secret + "\nuser: bob"},
		{"json value on the next line", "{\n  \"db_password\":\n    \"hunter2\"\n}", "{\n  \"db_password\":\n    \"" + secret + "\"\n}"},
		{"mapping under a secret key is walked", "x_secret:\n  name: db-creds\n  db_password: v-1", "x_secret:\n  name: db-creds\n  db_password: " + secret},

		// A line break between the key and the separator, as the password
		// rule allows: that rule's own match must survive.
		{"line break before the separator", "x_token=a-password\n=hunter2", "x_token=" + secret + "\n=" + secret},
		{"line break before the separator, longer key", "db_password\n  = hunter2", "db_password\n  = " + secret},

		// Arrays inside arrays, and triple quotes.
		{"nested array", `"credentials": ["a", ["hunter2"], "hunter3"], "n": 1`, `"credentials": [` + secret + `], "n": 1`},
		{"array nested four deep", `"credentials": [[[["hunter2"]]], "hunter3"], "n": 1`, `"credentials": [` + secret + `], "n": 1`},
		{"array nested six deep", `"token":[[[[[["hunter2"]]]]]], "n": 1`, `"token":[` + secret + `], "n": 1`},

		// An array wrapped after its first item, as Python's pprint and
		// Node's util.inspect print a long list, is followed to its closing
		// bracket; a string in an array ends at a line break and the array
		// goes on.
		{"wrapped array", "\"credentials\": [\"hunter2\",\n                \"hunter3\"], \"n\": 1", `"credentials": [` + secret + `], "n": 1`},
		{"wrapped array, single quotes", "{'credentials': ['hunter2',\n  'hunter3'],\n 'user': 'bob'}", "{'credentials': [" + secret + "],\n 'user': 'bob'}"},
		{"wrapped array over several lines", "credentials: [ 'hunter1',\n  'hunter2',\n  'hunter3' ]\nuser: bob", "credentials: [" + secret + "]\nuser: bob"},
		{"pretty array, a fourth bracket level", "\"credentials\": [\n  [[[\"a\"]]],\n  \"hunter2\"\n],\n\"n\": 1", "\"credentials\": [" + secret + "],\n\"n\": 1"},
		{"pretty array, nested on lines", "\"credentials\": [\n  [\n    [\n      [\n        \"hunter2\"\n      ]\n    ]\n  ],\n  \"hunter3\"\n],\n\"n\": 1", "\"credentials\": [" + secret + "],\n\"n\": 1"},
		{"pretty array, a string open at its line end", "\"credentials\": [\n  \"a,\n  \"hunter2\"\n],\n\"n\": 1", "\"credentials\": [" + secret + "],\n\"n\": 1"},
		{"pretty array, an apostrophe", "credentials: [\n  it's,\n  hunter2\n]\nn: 1", "credentials: [" + secret + "]\nn: 1"},
		{"wrapped array, never closed", "x_token=[hunter1 \nhunter2\nhunter3", "x_token=[" + secret},

		// A double- or single-quoted string ends at a line break: the lines
		// after it are not part of the value. A JSON string never holds a raw
		// line break, and an unterminated quote must not hide the lines after it.
		{"double-quoted string ends at a line break", "\"db_password\": \"hunter2\nnext: 1\"", "\"db_password\": \"" + secret + "\nnext: 1\""},
		{"single-quoted string ends at a line break", "db_password='hunter2\nnext: 1'", "db_password='" + secret + "\nnext: 1'"},
		{"text glued to the closing bracket", `x_token=[ab cd]ef next`, `x_token=[` + secret + ` next`},
		{"triple double quotes", `db_password = """hunter two""" next`, `db_password = """` + secret + `""" next`},
		{"triple single quotes", `db_password = '''hunter "two"''' next`, `db_password = '''` + secret + `''' next`},

		// Name/value pairs whose name is a secret key.
		{"name and value", `{"name":"DB_PASSWORD","value":"hunter2"}`, `{"name":"DB_PASSWORD","value":"` + secret + `"}`},
		{"key and value, capitalised", `{"Key": "api-token", "Value": "hunter2"}`, `{"Key": "api-token", "Value": "` + secret + `"}`},
		{"name and value in yaml", "- name: DB_PASSWORD\n  value: hunter2\n- name: PORT\n  value: 8080", "- name: DB_PASSWORD\n  value: " + secret + "\n- name: PORT\n  value: 8080"},
		{"name and value in a json string", `{\"name\":\"DB_PASSWORD\",\"value\":\"hunter 2\"}`, `{\"name\":\"DB_PASSWORD\",\"value\":\"` + secret + `\"}`},
		{"header name and value in yaml", "httpHeaders:\n- name: Authorization\n  value: Bearer hunter2\n- name: X-Trace\n  value: abc", "httpHeaders:\n- name: Authorization\n  value: Bearer " + secret + "\n- name: X-Trace\n  value: abc"},
		{"header name and value, basic", "- name: Authorization\n  value: Basic dXNlcjpodW50ZXIy hunter2", "- name: Authorization\n  value: Basic " + secret},
		{"header name and value, other scheme", "- name: Proxy-Authorization\n  value: Negotiate hunter2 x", "- name: Proxy-Authorization\n  value: " + secret},
		{"cookie name and value", "- name: Cookie\n  value: a=1; sid=hunter2\nnext: 1", "- name: Cookie\n  value: " + secret + "\nnext: 1"},
		{"header name and value in logfmt", "name=Authorization value=Bearer hunter2 x", "name=Authorization value=Bearer " + secret},
		{"name and valueFrom", "- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef: {name: db, key: password}", "- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef: {name: db, key: password}"},

		// A bare value in a query string or a ; list takes the pairs glued
		// to it: nothing of a following secret is left.
		{"query string", `https://h/login?access_token=abc&password=aB3,hunter2`, `https://h/login?access_token=` + secret},
		{"semicolon list", `ClientId=app;ClientSecret=abc;Password=aB3,hunter2;`, `ClientId=app;ClientSecret=` + secret},
		{"env with a comma", `POSTGRES_PASSWORD=aB3,hunter2`, `POSTGRES_PASSWORD=` + secret},
		{"describe form with a bracket", `DB_PASSWORD:  aB3)hunter2`, `DB_PASSWORD:  ` + secret},
		{"env list in json", `"Env":["POSTGRES_PASSWORD=aB3,hunter2","PATH=/bin"]`, `"Env":["POSTGRES_PASSWORD=` + secret},
		{"value in braces", `db_password={hunter2}`, `db_password=` + secret},

		// A value that holds a key word is still replaced whole.
		{"key word inside a quoted value", `{"db_password":"mysecret:2024"}`, `{"db_password":"` + secret},
		{"key word inside a bare value", `x_token: Xtoken: y`, `x_token: ` + secret + ` ` + secret},
		{"arrays in an array", `"token":[["a"],["hunter2"]]`, `"token":[` + secret + `]`},
		{"object on the key's line", `"token": {"value": "hunter2"}, "user": "bob"`, `"token": ` + secret},
		{"object opening at the end of the line", "\"x_secret\": {\n  \"name\": \"db-creds\",\n  \"api_token\": \"hunter2\"\n}", "\"x_secret\": {\n  \"name\": \"db-creds\",\n  \"api_token\": \"" + secret + "\"\n}"},

		// Cookies, more key names, and a URL with a password and no user.
		{"cookie header", `Cookie: session=hunter2; csrftoken=y`, `Cookie: ` + secret},
		{"set-cookie header", "Set-Cookie: sid=hunter2; Path=/; HttpOnly\nHost: x", "Set-Cookie: " + secret + "\nHost: x"},
		{"cookie in json", `{"headers":{"cookie":"sid=hunter2; a=b"},"n":1}`, `{"headers":{"cookie":"` + secret + `"},"n":1}`},
		{"signing key", `signing_key=hunter2 next`, `signing_key=` + secret + ` next`},
		{"encryption key", `ENCRYPTION_KEY: hunter2`, `ENCRYPTION_KEY: ` + secret},
		{"ssh key", `ssh_key: hunter2`, `ssh_key: ` + secret},
		{"secret key base", `secret_key_base: hunter2`, `secret_key_base: ` + secret},
		{"secret string", `"SecretString":"hunter 2"`, `"SecretString":"` + secret + `"`},
		{"url with a password and no user", `redis://:hunter2@host:6379/0`, `redis://:<REDACTED:basic_auth>@host:6379/0`},
		{"aws signature", `Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261123/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=hunter2`, `Authorization: ` + secret},

		// A header written as text runs to the end of its line, whatever
		// quotes it holds, unless it sits in a quoted string: then it ends
		// at the quote that closes that string.
		{"cookie holding json", `Cookie: prefs={"theme":"dark"}; session=hunter2`, `Cookie: ` + secret},
		{"cookie holding an apostrophe", `Cookie: name=O'Brien; sid=hunter2`, `Cookie: ` + secret},
		{"cookie holding an array", `Cookie: cart=["a","b"]; sid=hunter2`, `Cookie: ` + secret},
		{"cookie holding a quoted word", `cookie: lang=en; note=say "hi"; sid=hunter2`, `cookie: ` + secret},
		{"bearer in quotes", `Authorization: Bearer "hunter2"`, `Authorization: Bearer ` + secret},
		{"cookie in single quotes", `curl -H 'Cookie: a=b; sid=hunter2' https://h`, `curl -H 'Cookie: ` + secret + `' https://h`},
		{"cookie in single quotes with an apostrophe", `curl -H 'Cookie: name=O'Brien; sid=hunter2' https://h`, `curl -H 'Cookie: ` + secret + `' https://h`},
		{"header in a json string", `{"h":"Authorization: ApiKey hunter2","user":"bob"}`, `{"h":"Authorization: ` + secret + `","user":"bob"}`},
		{"header in an escaped json string", `{"msg":"curl -H \"Cookie: sid=hunter2\" https://x"}`, `{"msg":"curl -H \"Cookie: ` + secret + `\" https://x"}`},
		{"quoted header key, unquoted value", `"Authorization": ApiKey hunter2, "user": "bob"`, `"Authorization": ` + secret},

		// A comment after the separator goes with the value below it, and a
		// pair written in it is replaced too.
		{"pair in a comment after the separator", "token=# \"db_apikey\" \n : \"two words hunter2\" 0", "<REDACTED:password>\n : \"" + secret + "\" 0"},

		// A pair whose key goes with the value before it takes at least the
		// word after its separator, as the password rule did.
		{"glued after a quote in a cookie", `Cookie: token: "v"hunter2 next`, `Cookie: ` + secret},
		{"glued after a quote in a quoted value", `x_secret: " token: "v"hunter2 next`, `x_secret: "` + secret + ` next`},
		// A key in the word such a pair reaches over reaches over its own.
		{"key in the word a swallowed key reaches over", "a_token=b_token=\npassword=\n\nhunter2", "a_token=" + secret + "\n" + secret + "\n\n" + secret},
		{"key with its own value in a quoted value", `x_secret: 'pwd=[a]',hunter2 tail`, `x_secret: '` + secret + ` tail`},
		{"keys chained over lines", "pwd:pwd:\npwd:\nhunter2 next", "<REDACTED:password>\n" + secret + "\n" + secret + " next"},

		// Pairs the password rule already replaced whole keep that output.
		{"password rule", `password=hunter2 user=bob`, `<REDACTED:password> user=bob`},
		{"password rule, quoted value with spaces", `password="correct horse" user=bob`, `<REDACTED:password> user=bob`},
		{"password rule, prose", `the token: was invalid`, `the <REDACTED:password> invalid`},
	}
	redactor := newTestRedactor(t, false, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactor.Scrub(tc.in)
			if got != tc.want {
				t.Fatalf("Scrub(%q)\n got  %q\n want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactorKeepsValuesThatAreNotSecrets lists what the key/value rules
// must leave alone: keys that contain a secret word but do not end with it
// (counts, timestamps, names of secrets), booleans and nulls, objects, and
// prose with no separator.
func TestRedactorKeepsValuesThatAreNotSecrets(t *testing.T) {
	t.Parallel()
	lines := []string{
		`"token_count": 5`, `max_tokens: 4096`, `tokens=12`, `"prompt_tokens":812,"completion_tokens":64`,
		`password_changed_at: 2026-11-01T00:00:00Z`, `password_length=12`, `token_ttl=3600`, `tokenExpirationSeconds: 3607`,
		`secret_name: db-creds`, `secretName: db-creds`, `secretKeyRef: {name: db, key: password}`, `imagePullSecrets: [regcred]`,
		`"api_keys": ["a","b"]`, `passwords=3`, `secrets: 2`,
		`automountServiceAccountToken: false`, `"client_secret": null, "x": 1`, `has_password=true`, `"access_token":NULL}`, `x_token: True`,
		`"client_secret": "", "list": []`, `x_token: []`, `client_secret=`, "db_password:\nuser: bob",
		`the token was invalid`, `password reset requested`, `secret "db-creds" not found`, `cache miss for key: abc`, `primary_key=5`,
		`--token-auth-file=/etc/tokens.csv`, `--service-account-private-key-file=/etc/sa.key`, `authorization-mode=Node,RBAC`,
		`type: kubernetes.io/service-account-token`, `-----BEGIN CERTIFICATE-----`, `-----BEGIN PUBLIC KEY-----`,
	}
	for _, redactIPs := range []bool{false, true} {
		redactor := newTestRedactor(t, redactIPs, nil)
		for _, line := range lines {
			if got := redactor.Scrub(line); got != line {
				t.Errorf("Scrub(%q) = %q, want it unchanged", line, got)
			}
		}
	}
}

// TestRedactorRedactsProseAfterASecretKeyAndSeparator records the accepted
// cost of matching on the key: text that follows a secret key and a separator
// is replaced even when a person wrote it as prose.
func TestRedactorRedactsProseAfterASecretKeyAndSeparator(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, false, nil)
	for in, want := range map[string]string{
		`invalid credentials: user not found`: `invalid credentials: <REDACTED:secret> not found`,
		`bad access_token: expired`:           `bad access_token: <REDACTED:secret>`,
		`authorization: failed for bob`:       `authorization: <REDACTED:secret>`,
		`"credentials": {"user":"bob"}`:       `"credentials": <REDACTED:secret>`,
	} {
		if got := redactor.Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

const canary = "Zq9canaryValue7Xw"

// canaryValue is one way to write a value that holds the canary.
type canaryValue struct {
	open, close string
	// authorizationOnly marks values of several words with no quotes, which
	// only an Authorization, Cookie or Set-Cookie header takes whole.
	authorizationOnly bool
	// ownLine marks values written on the lines after the key: the key must
	// then start its line, as the value is told apart by its indentation.
	ownLine bool
	// afterBlank marks the folded block scalar, whose > indicator reads as
	// part of the separator (=>) unless a blank comes before it.
	afterBlank bool
}

var (
	canaryKeys = []string{
		"password", "passwd", "pwd", "passphrase", "secret", "token", "credential", "credentials",
		"api_key", "apikey", "api-key", "access_key", "access-key", "accesskey", "secret_access_key", "secret_key",
		"client_secret", "private_key", "privatekey", "signing_key", "encryption_key", "ssh_key", "secret_key_base", "SecretString",
		"authorization", "cookie", "Set-Cookie",
	}
	canaryKeyPrefixes = []string{"", "db_", "aws_", "x-", "my", "app.", "DB_"}
	canarySeparators  = []string{":", "=", ": ", " = ", " : ", "=>", " => ", ":=", "==", ":\t", "\n=", " \n : "}
	canaryValues      = []canaryValue{
		{open: "", close: ""}, {open: `"`, close: `"`}, {open: `'`, close: `'`}, {open: `\"`, close: `\"`}, {open: `\\\"`, close: `\\\"`},
		{open: `"with space `, close: ` and more"`}, {open: `'with space `, close: ` and more'`}, {open: `\"with space `, close: ` and more\"`},
		{open: `"esc\" `, close: `"`}, {open: `'it''s `, close: `'`}, {open: `[`, close: `]`}, {open: `["a b", "`, close: `"]`},
		{open: `{SSHA}`, close: ``}, {open: `\`, close: ``}, {open: `"`, close: ``}, {open: `'`, close: ``},
		// punctuation a bare value may hold
		{open: `aB3}`, close: `,qq`}, {open: `)`, close: ``}, {open: `{`, close: ``}, {open: `a,`, close: `)]}`}, {open: `]`, close: `;x`}, {open: `<`, close: `>`},
		// arrays in arrays, text glued to a bracket, triple quotes
		{open: `["a", ["`, close: `"], "b"]`}, {open: `[[[["`, close: `"]]]]`}, {open: `[a b]`, close: ``}, {open: `"""two words `, close: `"""`}, {open: `'''two words `, close: `'''`},
		// a value on the line after the key
		{open: "\n  \"", close: "\""}, {open: "\n\t'two words ", close: "'"},
		// YAML block scalars and sequences
		{open: "|\n  ", close: "\n", ownLine: true}, {open: ">-\n    first line\n\n    ", close: " more\n", ownLine: true, afterBlank: true}, {open: "|2+ # note\n   two words ", close: "\n", ownLine: true},
		{open: "\n  - ", close: "\n", ownLine: true}, {open: "\n- first\n- ", close: "\n", ownLine: true}, {open: "\n  ", close: "\n", ownLine: true},
		// Authorization with a scheme the rule does not know
		{open: "SSWS ", close: "", authorizationOnly: true}, {open: "ApiKey ", close: "==", authorizationOnly: true}, {open: "Negotiate ", close: " trailer", authorizationOnly: true},
		{open: `Digest username="bob", response="`, close: `"`, authorizationOnly: true}, {open: `Digest username="bob", response="x", cnonce="`, close: `", qop=auth`, authorizationOnly: true},
	}
	canaryKeyQuotes = []struct{ open, close string }{{"", ""}, {`"`, `"`}, {`'`, `'`}, {`\"`, `\"`}}
	// canaryNamedPairs write the key as the name of a name/value pair. The
	// first %s is the key, the second the canary.
	canaryNamedPairs = []string{
		`{"name":"%s","value":"%s"}`, `{"name": "%s", "value": "two words %s"}`, `{"Key":"%s","Value":"%s"}`, `{"key":"%s","value":%s}`,
		"- name: %s\n  value: %s\n", "- name: \"%s\"\n  value: \"two words %s\"\n", `name=%s value=%s`, `{\"name\":\"%s\",\"value\":\"two words %s\"}`,
		"name: %s\nvalue: |\n  two words %s\n",
	}
)

// plantedPair writes the canary as the value of a secret key. ok is false
// for a combination that is not a secret written under that key: several
// bare words under a key other than Authorization, or a folded block scalar
// glued to the separator.
func plantedPair(key, prefix, separator string, keyQuote, valueStyle int) (pair string, ownLine, ok bool) {
	quote := canaryKeyQuotes[keyQuote%len(canaryKeyQuotes)]
	style := canaryValues[valueStyle%len(canaryValues)]
	if style.authorizationOnly && key != "authorization" && key != "cookie" && key != "Set-Cookie" {
		return "", false, false
	}
	if style.afterBlank && !strings.HasSuffix(separator, " ") && !strings.HasSuffix(separator, "\t") {
		return "", false, false
	}
	return quote.open + prefix + key + quote.close + separator + style.open + canary + style.close, style.ownLine, true
}

// canarySurroundings are texts a pair is planted in. The last one nests
// more pairs than the scanner follows at once.
var canarySurroundings = []struct{ before, after string }{
	{"", ""}, {"level=info msg=start ", " user=bob"}, {`{"user":"bob",`, `,"n":1}`}, {"user: bob\n", "\nport: 5432"},
	{`msg="bad token: " `, ` user="bob"`}, {"x_secret: ", ""}, {"token= ", " next"}, {"Authorization: Basic abc ", ""},
	{strings.Repeat("k_token=[", 40) + "] ", " user=bob"},
}

// TestRedactorRemovesPlantedSecret plants a canary as the value of every
// secret key, with every prefix, separator, key quoting and value style, in
// the middle of a line, and requires the canary to be gone. It looks at the
// output only: no reference redactor takes part.
func TestRedactorRemovesPlantedSecret(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, true, shippedExtraPatterns)
	for keyIndex, key := range canaryKeys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			planted := 0
			check := func(line string) {
				planted++
				if got := redactor.Scrub(line); strings.Contains(got, canary) {
					t.Errorf("Scrub(%q) = %q still holds the planted secret", line, got)
				}
			}
			for prefixIndex, prefix := range canaryKeyPrefixes {
				for _, separator := range canarySeparators {
					for keyQuote := range canaryKeyQuotes {
						for valueStyle := range canaryValues {
							pair, ownLine, ok := plantedPair(key, prefix, separator, keyQuote, valueStyle)
							if !ok {
								continue
							}
							for aroundIndex, around := range canarySurroundings {
								// Every pair is tried bare and in one
								// surrounding; every surrounding sees every
								// key, separator, quoting and value style.
								if aroundIndex != 0 && aroundIndex != 1+(keyIndex+prefixIndex)%(len(canarySurroundings)-1) {
									continue
								}
								before := around.before
								if ownLine && before != "" {
									before += "\n"
								}
								check(before + pair + around.after)
							}
						}
					}
				}
				for _, form := range canaryNamedPairs {
					for _, around := range canarySurroundings {
						before := around.before
						if before != "" {
							before += "\n"
						}
						check(before + fmt.Sprintf(form, prefix+key, canary) + around.after)
					}
				}
			}
			if planted == 0 {
				t.Fatal("no pair was planted")
			}
		})
	}
}

// TestRedactorFailsClosedBehindDeeplyNestedPairs covers text that nests more
// pairs than the scanner follows at once: the line is replaced from the
// first value it stopped following to its end, so a secret behind it cannot
// be left with its key consumed; the pairs on the next line are found
// afresh, and the lines after it are kept.
func TestRedactorFailsClosedBehindDeeplyNestedPairs(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, false, nil)
	line := strings.Repeat("k_token=[", 20) + "]" + " x_token: \" db_password=\"hunter2 x\" user=bob"
	got := redactor.Scrub(line)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Scrub(%q) = %q still holds the secret", line, got)
	}
	if want := "k_token=[<REDACTED:secret>"; got != want {
		t.Fatalf("Scrub(%q) = %q, want %q", line, got, want)
	}
	// Values that end on their line are the ones dropped at the limit, so
	// the next line is kept; arrays that are never closed run to the end.
	for _, exhausting := range []struct {
		prefix   string
		keepNext bool
	}{{strings.Repeat("token:", 30) + " ", true}, {strings.Repeat("k_token=[", 30) + " ", false}} {
		line := exhausting.prefix + `x_secret: password: "hunter2 x"` + "\nnext: ok"
		got := redactor.Scrub(line)
		if strings.Contains(got, "hunter2") || strings.HasSuffix(got, "\nnext: ok") != exhausting.keepNext {
			t.Errorf("Scrub(%q) = %q, want the secret gone and the next line kept: %v", line, got, exhausting.keepNext)
		}
	}
	// A line of nested pairs that ends in a key, with a pair on the next
	// line whose key is quoted.
	for _, unit := range []string{"a_token=", "a_token: ", `"a_token":`, "pwd="} {
		for _, next := range []string{`"db_password": "hunter2 Y",`, `'db_password' => 'hunter2 Y'`, "\"db_password\": |\n  hunter2", `"password": "hunter2 Y"`} {
			for _, count := range []int{1, 15, 16, 17, 30, 300} {
				line := strings.Repeat(unit, count) + "\n" + next + "\nnext: ok"
				if got := redactor.Scrub(line); strings.Contains(got, "hunter2") || !strings.HasSuffix(got, "\nnext: ok") {
					t.Errorf("Scrub(%d x %q, then %q) = %q, want the secret gone and the last line kept", count, unit, next, got)
				}
			}
		}
	}
	for _, prefix := range []string{
		strings.Repeat("k_token=[", 3000), strings.Repeat(`k_token=" `, 3000), strings.Repeat("k_token: |\n", 3000),
		strings.Repeat("k_token=k_token=", 3000), strings.Repeat("authorization: k_token=", 3000),
	} {
		for _, pair := range []string{` db_password="` + canary + ` x"`, " api_secret=" + canary, "\nclient_secret: |\n  " + canary + "\n"} {
			if got := redactor.Scrub(prefix + pair + " user=bob"); strings.Contains(got, canary) {
				t.Errorf("behind %.20q...: Scrub(...%q) = ...%.80q still holds the planted secret", prefix, pair, got[max(0, len(got)-80):])
			}
		}
	}
}

// TestRedactorKeepsFollowingMultiLineValuesAtTheLimit puts a line of more
// pairs than the scanner follows at once inside a value that spans lines:
// the pairs on that line end there and are the ones dropped, so the lines
// of the outer value after it are still replaced. When every value followed
// can span lines, the text is replaced from the first of them to its end.
func TestRedactorKeepsFollowingMultiLineValuesAtTheLimit(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, false, nil)
	dense := func(count int) string {
		var pairs strings.Builder
		for index := range count {
			fmt.Fprintf(&pairs, "k%d_token=v%d&", index, index)
		}
		return pairs.String()
	}
	const first, second = "Zq9hunterA", "Wk4hunterB"
	for _, count := range []int{15, 16, 17, 18, 40} {
		line := dense(count)
		for _, shape := range []struct{ name, text string }{
			{"block scalar", "credentials: |\n  " + line + "\n  " + first + "\n  " + second + "\nafter: 1"},
			{"block scalar, dense line after a secret line", "db_password: |\n  " + first + "\n  " + line + "\n  " + second + "\nafter: 1"},
			{"pretty array", "\"credentials\": [\n  \"https://h/?" + line + "\",\n  \"" + first + "\",\n  \"" + second + "\"\n],\n\"after\": 1"},
			{"triple quote", "x_secret=\"\"\"\n" + line + "\n" + first + "\n" + second + "\"\"\" after"},
			{"lines after the key", "credentials:\n  " + line + "\n  " + first + "\n  " + second + "\nafter: 1"},
			{"list after the key", "credentials:\n  - " + line + "\n  - " + first + "\n  - " + second + "\nafter: 1"},
		} {
			got := redactor.Scrub(shape.text)
			if strings.Contains(got, first) || strings.Contains(got, second) {
				t.Errorf("%s, %d pairs: Scrub(%q) = %q keeps a secret line", shape.name, count, shape.text, got)
			}
			if !strings.HasSuffix(got, "after: 1") && !strings.HasSuffix(got, "\"after\": 1") && !strings.HasSuffix(got, " after") {
				t.Errorf("%s, %d pairs: Scrub(%q) = %q replaces the text after the value", shape.name, count, shape.text, got)
			}
		}
	}
	for _, unit := range []string{"a_token=[\n", "a_token=\"\"\" ", "a_token: |\n "} {
		for _, count := range []int{16, 17, 40} {
			text := "user=bob " + strings.Repeat(unit, count) + "db_password: \"" + first + " " + second + "\"\nlast line"
			if got := redactor.Scrub(text); strings.Contains(got, first) || strings.Contains(got, second) {
				t.Errorf("%d x %q: Scrub(%q) = %q keeps a secret", count, unit, text, got)
			}
		}
	}
	// The pairs dropped at the limit take the rest of their line with them,
	// so the secret after them in the value that holds them is gone.
	for _, count := range []int{17, 20, 40} {
		text := `db_password="` + first + " " + dense(count) + " " + second + `" user=bob`
		if got := redactor.Scrub(text); strings.Contains(got, first) || strings.Contains(got, second) {
			t.Errorf("%d pairs in a quoted value: Scrub(%q) = %q keeps a secret", count, text, got)
		}
	}
	// A wrapped array is followed over a bounded number of lines; past
	// them the value runs to the end of the text.
	for _, lines := range []int{maxWrappedArrayLines, maxWrappedArrayLines + 1} {
		text := "credentials: [\"" + first + "\",\n" + strings.Repeat("  \"x\",\n", lines-1) + "  \"" + second + "\"]\nafter: 1"
		got := redactor.Scrub(text)
		if strings.Contains(got, first) || strings.Contains(got, second) {
			t.Errorf("wrapped array over %d lines keeps a secret: %.80q", lines, got)
		}
		if closed := lines <= maxWrappedArrayLines; strings.HasSuffix(got, "]\nafter: 1") != closed {
			t.Errorf("wrapped array over %d lines: Scrub(...) = ...%q, want it followed to its bracket: %v", lines, got[max(0, len(got)-40):], closed)
		}
	}
	text := "user=bob " + strings.Repeat("a_token=[\n", 17) + "db_password: \"" + first + "\"\nlast line"
	if got, want := redactor.Scrub(text), "user=bob a_token=[<REDACTED:secret>"; got != want {
		t.Errorf("17 open arrays: Scrub(%q) = %q, want %q", text, got, want)
	}
}

// FuzzRedactorRemovesPlantedSecret plants the canary after a secret key
// between arbitrary text and requires the output not to contain it, the
// redactor not to panic, and every rule's condition to hold where the rule
// matches. It looks at the output only: no reference redactor takes part.
func FuzzRedactorRemovesPlantedSecret(f *testing.F) {
	f.Add("", uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), "")
	f.Add(`msg="bad token: "`, uint8(1), uint8(1), uint8(1), uint8(1), uint8(5), ` user="bob"`)
	f.Add("x_secret:", uint8(4), uint8(2), uint8(2), uint8(0), uint8(0), "\nnext: 1")
	f.Add("Authorization: Basic", uint8(19), uint8(0), uint8(2), uint8(1), uint8(1), "}")
	f.Add("-----BEGIN PRIVATE KEY-----", uint8(17), uint8(3), uint8(0), uint8(3), uint8(3), "-----END PRIVATE KEY-----")
	f.Add(strings.Repeat("k_token=[", 20)+"]", uint8(0), uint8(1), uint8(1), uint8(0), uint8(5), " user=bob")
	f.Add("user: bob", uint8(0), uint8(1), uint8(2), uint8(0), uint8(29), "port: 1")
	f.Add("", uint8(19), uint8(0), uint8(2), uint8(0), uint8(38), " next")
	f.Add("x", uint8(2), uint8(0), uint8(10), uint8(0), uint8(16), "")
	// A line of nested pairs ending in a key, before the planted pair.
	f.Add(strings.Repeat("a_token=", 30), uint8(0), uint8(1), uint8(2), uint8(1), uint8(1), ",")
	f.Add(strings.Repeat("a_token=", 30), uint8(0), uint8(1), uint8(10), uint8(2), uint8(5), "")
	redactor := newTestRedactor(f, true, shippedExtraPatterns)
	f.Fuzz(func(t *testing.T, before string, key, prefix, separator, keyQuote, valueStyle uint8, after string) {
		if strings.Contains(before, canary) || strings.Contains(after, canary) {
			t.Skip("the surrounding text holds the canary itself")
		}
		name := canaryKeyPrefixes[int(prefix)%len(canaryKeyPrefixes)] + canaryKeys[int(key)%len(canaryKeys)]
		pair, ownLine, ok := plantedPair(canaryKeys[int(key)%len(canaryKeys)], canaryKeyPrefixes[int(prefix)%len(canaryKeyPrefixes)],
			canarySeparators[int(separator)%len(canarySeparators)], int(keyQuote), int(valueStyle))
		if !ok {
			// Use the slot for a name/value pair instead.
			pair, ownLine = fmt.Sprintf(canaryNamedPairs[int(valueStyle)%len(canaryNamedPairs)], name, canary), true
		}
		line := before + " " + pair + " " + after
		if ownLine {
			line = before + "\n" + pair + "\n" + after
		}
		got := redactor.Scrub(line)
		if strings.Contains(got, canary) {
			t.Fatalf("Scrub(%q) = %q still holds the planted secret", line, got)
		}
		for _, rule := range redactor.rules {
			if ruleReplaces(rule, line) && !rule.need.holds(line) {
				t.Fatalf("rule %s matches %q but its condition does not hold", rule.name, line)
			}
		}
	})
}

func TestScrubFieldsReplacesValuesUnderSecretKeys(t *testing.T) {
	t.Parallel()
	redactor := newTestRedactor(t, false, nil)
	fields := map[string]interface{}{
		"password":      "hunter2",
		"DB_PASSWORD":   "hunter2",
		"x-api-key":     "k-1",
		"clientSecret":  "s-1",
		"pin_token":     1234,
		"Authorization": "Bearer abc",
		"credentials":   []interface{}{"a", 7, true, map[string]interface{}{"user": "bob", "secret_access_key": "s-2"}},
		"secret":        map[string]interface{}{"name": "db-creds", "token": "t-1"},
		"token_count":   5,
		"max_tokens":    "4096",
		"secret_name":   "db-creds",
		"has_token":     true,
		"api_key":       nil,
		"msg":           `login {"password":"hunter2"} db_password=hunter2 Authorization: Basic abc`,
		"list":          []interface{}{`{"token": "abc123"}`, 7},
		"env": []interface{}{
			map[string]interface{}{"name": "DB_PASSWORD", "value": "hunter2"},
			map[string]interface{}{"name": "PORT", "value": "8080"},
			map[string]interface{}{"Key": "api-token", "Value": 1234, "Other": "kept"},
			map[string]interface{}{"name": "Authorization", "value": []interface{}{"Bearer abc"}},
		},
	}
	want := map[string]interface{}{
		"password":      "<REDACTED:secret>",
		"DB_PASSWORD":   "<REDACTED:secret>",
		"x-api-key":     "<REDACTED:secret>",
		"clientSecret":  "<REDACTED:secret>",
		"pin_token":     "<REDACTED:secret>",
		"Authorization": "<REDACTED:secret>",
		"credentials":   []interface{}{"<REDACTED:secret>", "<REDACTED:secret>", true, map[string]interface{}{"user": "bob", "secret_access_key": "<REDACTED:secret>"}},
		"secret":        map[string]interface{}{"name": "db-creds", "token": "<REDACTED:secret>"},
		"token_count":   5,
		"max_tokens":    "4096",
		"secret_name":   "db-creds",
		"has_token":     true,
		"api_key":       nil,
		"msg":           `login {"password":"<REDACTED:secret>"} db_password=<REDACTED:secret> Authorization: Basic <REDACTED:secret>`,
		"list":          []interface{}{`{"token": "<REDACTED:secret>"}`, 7},
		"env": []interface{}{
			map[string]interface{}{"name": "DB_PASSWORD", "value": "<REDACTED:secret>"},
			map[string]interface{}{"name": "PORT", "value": "8080"},
			map[string]interface{}{"Key": "api-token", "Value": "<REDACTED:secret>", "Other": "kept"},
			map[string]interface{}{"name": "Authorization", "value": []interface{}{"<REDACTED:secret>"}},
		},
	}
	got := redactor.ScrubFields(fields)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScrubFields\n got  %#v\n want %#v", got, want)
	}
	if fields["password"] != "hunter2" {
		t.Fatal("ScrubFields changed its input")
	}
}

// TestFoldedPrefixAcceptsEveryFoldVariant ties the folded literal matching to
// foldVariants: each spelling foldVariants lists for a letter is accepted, in
// every position, and nothing else is.
func TestFoldedPrefixAcceptsEveryFoldVariant(t *testing.T) {
	t.Parallel()
	for letter := byte('a'); letter <= 'z'; letter++ {
		for _, literal := range []string{string(letter), "x" + string(letter), string(letter) + "x", "x" + string(letter) + "x"} {
			for _, variant := range foldVariants(letter) {
				value := strings.Replace(literal, string(letter), variant, 1)
				if foldedPrefix(value+" tail", literal) != len(value) {
					t.Errorf("foldedPrefix(%q, %q) does not cover the whole spelling", value, literal)
				}
				if len(value) > 1 && foldedPrefix(value[:len(value)-1], literal) >= 0 {
					t.Errorf("foldedPrefix(%q, %q) accepts a cut value", value[:len(value)-1], literal)
				}
			}
		}
		accepted := map[string]bool{}
		for _, variant := range foldVariants(letter) {
			accepted[variant] = true
		}
		for r := rune(0); r < 0x3000; r++ {
			if got := foldedPrefix(string(r), string(letter)) >= 0; got != accepted[string(r)] {
				t.Errorf("foldedPrefix(%q, %q) = %t, want %t", string(r), string(letter), got, accepted[string(r)])
			}
		}
	}
}

// TestScrubJSONWalksNamesAndSecretFields covers the walk the egress guard
// uses: field names go through the scrubber too, a value under a secret name
// is replaced whatever the scrubber is, and the result says whether anything
// changed.
func TestScrubJSONWalksNamesAndSecretFields(t *testing.T) {
	t.Parallel()
	scrub := func(value string) string { return strings.ReplaceAll(value, "bob", "BOB") }
	cases := []struct {
		name     string
		in, want interface{}
		changed  bool
	}{
		{"string", "hi bob", "hi BOB", true},
		{"clean string", "hi", "hi", false},
		{"scalars", []interface{}{1.5, true, nil}, []interface{}{1.5, true, nil}, false},
		{"field name", map[string]interface{}{"bob": 1.0}, map[string]interface{}{"BOB": 1.0}, true},
		{"secret field", map[string]interface{}{"password": "hunter2"}, map[string]interface{}{"password": "<REDACTED:secret>"}, true},
		{"secret field, already replaced", map[string]interface{}{"password": "<REDACTED:secret>"}, map[string]interface{}{"password": "<REDACTED:secret>"}, false},
		{"secret field, number", map[string]interface{}{"pin_token": 1234.0}, map[string]interface{}{"pin_token": "<REDACTED:secret>"}, true},
		{"secret field, kept literals", map[string]interface{}{"token": nil, "has_secret": true}, map[string]interface{}{"token": nil, "has_secret": true}, false},
		{"not a secret field", map[string]interface{}{"token_count": 5.0}, map[string]interface{}{"token_count": 5.0}, false},
		{
			"nested",
			[]interface{}{map[string]interface{}{"credentials": []interface{}{"a", map[string]interface{}{"user": "bob", "api_key": "k"}}}},
			[]interface{}{map[string]interface{}{"credentials": []interface{}{"<REDACTED:secret>", map[string]interface{}{"user": "BOB", "api_key": "<REDACTED:secret>"}}}},
			true,
		},
	}
	for _, tc := range cases {
		got, changed := ScrubJSON(tc.in, scrub)
		if !reflect.DeepEqual(got, tc.want) || changed != tc.changed {
			t.Errorf("%s: ScrubJSON = %#v, %t; want %#v, %t", tc.name, got, changed, tc.want, tc.changed)
		}
	}
}

// TestScrubFieldAndLabelUseTheFieldName covers the forms connectors use for
// one value held under a name: a nested value, and a string in a string map.
func TestScrubFieldAndLabelUseTheFieldName(t *testing.T) {
	t.Parallel()
	scrub := func(value string) string { return strings.ReplaceAll(value, "bob", "BOB") }
	if got := ScrubLabel("db_password", "hunter2", scrub); got != "<REDACTED:secret>" {
		t.Errorf("ScrubLabel under a secret name = %q", got)
	}
	if got := ScrubLabel("Authorization", "Basic abc", scrub); got != "<REDACTED:secret>" {
		t.Errorf("ScrubLabel under authorization = %q", got)
	}
	if got := ScrubLabel("token_count", "bob 5", scrub); got != "BOB 5" {
		t.Errorf("ScrubLabel under another name = %q", got)
	}
	got := ScrubField("user.credentials", []interface{}{"a", map[string]interface{}{"bob": "bob", "api_key": 7}}, scrub)
	want := []interface{}{"<REDACTED:secret>", map[string]interface{}{"bob": "BOB", "api_key": "<REDACTED:secret>"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ScrubField = %#v, want %#v", got, want)
	}
	if got := ScrubField("count", 5, scrub); got != 5 {
		t.Errorf("ScrubField of a number under another name = %#v", got)
	}
	// The value field of a map that names a secret, in a string map and in
	// a decoded JSON object.
	labels := map[string]string{"name": "DB_PASSWORD", "value": "hunter2", "other": "bob"}
	if got := ScrubLabelIn(labels, "value", labels["value"], scrub); got != "<REDACTED:secret>" {
		t.Errorf("ScrubLabelIn of the value of a named secret = %q", got)
	}
	if got := ScrubLabelIn(labels, "other", labels["other"], scrub); got != "BOB" {
		t.Errorf("ScrubLabelIn of another field = %q", got)
	}
	plain := map[string]interface{}{"Key": "port", "Value": 8080.0}
	if SecretFieldIn(plain, "Value") || !SecretFieldIn(map[string]interface{}{"Key": "api-token", "Value": 1.0}, "Value") || !SecretFieldIn(plain, "Cookie") {
		t.Error("SecretFieldIn misjudges a value field or a header name")
	}
	if got := ScrubFieldIn(map[string]interface{}{"key": "Cookie", "value": 7}, "value", 7, scrub); got != "<REDACTED:secret>" {
		t.Errorf("ScrubFieldIn of a number under a named header = %#v", got)
	}
}

// The tests below compare with the reference only: the redactor as it was
// before the key/value rules, frozen in redactor_test.go. Whatever that
// redactor removes from a line, the redactor must remove too. The extended
// reference takes no part, so a mistake shared by the added rules and their
// frozen copy shows here.

// linePieces are the pieces generated lines are built from. An empty piece
// stands for a fresh marker value, unique in its line.
var linePieces = []string{
	"password", "token", "secret", "api_key", "pwd", "passwd", "credentials", "authorization", "Authorization", "Bearer", "Basic", "bearer",
	"cookie", "Set-Cookie", "name", "value", "key", "signing_key", "SecretString",
	"db_", "x-", "X", "_", ".", "-", ":", "=", ":", "=", `"`, `"`, `'`, `\"`, `\`, " ", " ", " ", "\t", "\n", "\n  ", ",", "[", "]", "{", "}", "(", ")", ">", "<", ";", "&", "|", "- ", `"""`,
	"true", "null", "false", "-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----", "eyJ", "://", "@", "<REDACTED:secret>", "<REDACTED:password>",
	"+", "/", "%", "~", "user", "msg", "1", "a.b", "AKIA", "sk-", "example.com", "ſ", "K", "<REDACTED:bearer>",
	// No user agent opener ("Mozilla/5.0 ("): the user_agent rule, which
	// runs last, takes everything up to the closing bracket, and a secret
	// value that holds that bracket is replaced first. The value is gone
	// either way; what stays is the text of the user agent.
	"token:token:token:token:token:token:token:token:", "k_token=[k_token=[k_token=[k_token=[", `x_token="x_token="x_token="`,
	"", "", "", "", "", "", "", "",
	// A short quoted value, for text glued to its closing quote.
	`"v"`, `'v'`, `: "v"`,
}

// lineFromPieces joins the pieces picked by picks and returns the line with
// the marker values planted in it.
func lineFromPieces(picks []int) (string, []string) {
	var line strings.Builder
	var planted []string
	for _, pick := range picks {
		piece := linePieces[pick%len(linePieces)]
		if piece == "" {
			piece = fmt.Sprintf("Qz7c%dW", len(planted))
			planted = append(planted, piece)
		}
		line.WriteString(piece)
	}
	return line.String(), planted
}

// keptAfterReferenceRemoved returns a planted value the reference removed
// from the line and the redactor kept, or "".
func keptAfterReferenceRemoved(redactor *Redactor, reference *referenceRedactor, line string, planted []string) string {
	got, before := redactor.Scrub(line), reference.Scrub(line)
	for _, value := range planted {
		if strings.Contains(got, value) && !strings.Contains(before, value) {
			return value
		}
	}
	return ""
}

// TestRedactorNeverKeepsWhatTheReferenceRemoved builds lines from random
// pieces (keys, separators, quotes, brackets, line breaks, prefixes that nest
// pairs deeply) with marker values in them, and requires every marker value
// the reference removes to be gone from the redactor's output too.
func TestRedactorNeverKeepsWhatTheReferenceRemoved(t *testing.T) {
	t.Parallel()
	for index, pair := range redactorPairs(t) {
		t.Run(pair.name, func(t *testing.T) {
			t.Parallel()
			r := rand.New(rand.NewSource(int64(20261123 + index)))
			lines := 60000
			if testing.Short() {
				lines = 15000
			}
			failures := 0
			for range lines {
				picks := make([]int, 2+r.Intn(16))
				for i := range picks {
					picks[i] = r.Intn(len(linePieces))
				}
				line, planted := lineFromPieces(picks)
				if value := keptAfterReferenceRemoved(pair.redactor, pair.reference, line, planted); value != "" {
					t.Errorf("Scrub(%q) = %q keeps %s, which the reference removes: %q", line, pair.redactor.Scrub(line), value, pair.reference.Scrub(line))
					if failures++; failures == 20 {
						t.Fatal("stopping after 20 lines")
					}
				}
			}
		})
	}
}

// TestRedactorKeepsNothingTheReferenceRemovedBehindNestedPairs covers a line
// that nests more pairs than the scanner follows at once, in front of
// pairs the reference redacts: on that line and on the lines after it, the
// redactor still removes what the reference removes, and it does not replace
// the lines after it wholesale.
func TestRedactorKeepsNothingTheReferenceRemovedBehindNestedPairs(t *testing.T) {
	t.Parallel()
	redactor, reference := newTestRedactor(t, false, nil), newReferenceRedactor(t, false, nil)
	victims := []string{
		`x_secret: password: "Qz7c0W x"`, `msg="bad x_token: " password="Qz7c0W x" user=bob`, `x_token: db_password="Qz7c0W Qz7c1W"`,
		`Authorization: db_password="Qz7c0W Qz7c1W"`, `x_token= "api_secret": "Qz7c0W Qz7c1W"`, `password=Qz7c0W`, `secret: Qz7c0W token=Qz7c1W`,
		// A key inside another key's value, with its own value on the next
		// line and not indented: the password rule takes that word.
		"credentials:pwd=\nQz7c0W", "Authorization:pwd=\nQz7c0W", "x_token=token:\n\n  Qz7c0W Qz7c1W",
	}
	planted := []string{"Qz7c0W", "Qz7c1W"}
	for _, victim := range victims {
		for _, unit := range []string{"token:", "k_token=[", `x_token="`, "pwd=", "authorization: token=", "secret:\n"} {
			for _, count := range []int{0, 10, 20, 30, 60, 600} {
				line := strings.Repeat(unit, count) + "x " + victim
				if value := keptAfterReferenceRemoved(redactor, reference, line, planted); value != "" {
					t.Errorf("Scrub(%.80q... %q) = ...%.120q keeps %s, which the reference removes", line, victim, redactor.Scrub(line), value)
				}
			}
		}
		text := "GET /?q=" + strings.Repeat("token:", 600) + "x 200\n" + strings.Repeat("level=info msg=ok user=bob path=/healthz\n", 400) + victim + "\n"
		got := redactor.Scrub(text)
		for _, value := range planted {
			if strings.Contains(got, value) && strings.Contains(victim, value) && !strings.Contains(reference.Scrub(text), value) {
				t.Errorf("behind a line of nested pairs, %q keeps %s", victim, value)
			}
		}
		if kept := strings.Count(got, "level=info msg=ok user=bob path=/healthz\n"); kept != 400 {
			t.Errorf("behind a line of nested pairs, %d of the 400 ordinary lines are left", kept)
		}
	}
}

// FuzzRedactorNeverKeepsWhatTheReferenceRemoved is the same comparison on
// lines the fuzzer builds: each input byte picks a piece.
func FuzzRedactorNeverKeepsWhatTheReferenceRemoved(f *testing.F) {
	f.Add([]byte{0, 26, 76})
	f.Add([]byte{73, 73, 73, 34, 5, 0, 25, 34, 29, 76, 34, 76, 29})
	f.Add([]byte{1, 22, 75, 0, 26, 76, 40, 77})
	redactor := newTestRedactor(f, true, shippedExtraPatterns)
	reference := newReferenceRedactor(f, true, shippedExtraPatterns)
	plain, plainReference := newTestRedactor(f, false, nil), newReferenceRedactor(f, false, nil)
	f.Fuzz(func(t *testing.T, data []byte) {
		picks := make([]int, len(data))
		for i, b := range data {
			picks[i] = int(b)
		}
		line, planted := lineFromPieces(picks)
		if value := keptAfterReferenceRemoved(redactor, reference, line, planted); value != "" {
			t.Fatalf("Scrub(%q) = %q keeps %s, which the reference removes: %q", line, redactor.Scrub(line), value, reference.Scrub(line))
		}
		if value := keptAfterReferenceRemoved(plain, plainReference, line, planted); value != "" {
			t.Fatalf("Scrub(%q) = %q keeps %s, which the reference removes: %q", line, plain.Scrub(line), value, plainReference.Scrub(line))
		}
	})
}

// TestRedactorExaminesLinearText counts the bytes the key/value scanner
// examines on text built from one short unit repeated, for a grid of units:
// a key, what opens its value and what ends the unit, and units of two such
// pairs. The count per byte of text must stay under a fixed bound at every
// size from 4 KiB to 64 KiB (a random sample of the units at 1 KiB and
// 4 KiB in short mode): the work grows with the text, never with the text times the
// number of keys in it. No clock is involved.
func TestRedactorExaminesLinearText(t *testing.T) {
	t.Parallel()
	keys := []string{`"token":`, `token=`, `x_token: `, `"cookie":`, `Cookie: `, `Authorization: `, `'Authorization: `, `"Cookie: `, `\"Cookie: `, "name: token\nvalue: ", `"name":"token","value":`, `{"credentials":`, `authorization: bearer `, `token `, "token\n:", `key="token" value=`}
	openers := []string{`[`, `["`, `["a`, `[[`, `[[[`, `[[[[`, `[{"token":"a","x":`, `"`, `'`, `"""`, `'''`, `\"`, `\\"`, `{`, `|`, `>`, ``, `[it's`, `["a","`, `[ "a" `,
		`[\"`, `["\`, `["\"`, `['\`, `[\'`, `["a\"`, `[\\"`, `["a\\`, `[']`, `["]`, `[']["`, `[]`, `[[]`, `[[]]`, `["a"]x`, `"a"x`, `'a'x`, `"a\`, `# x`, `| # x`, `#`, `{"a":`, `{}`, `[{`, `[}`, "\t", `a`, `=`, `:`, `""`, `''`, `"a" "`, `a="b`, `a='b`, `a="b" c='d`}
	ends := []string{"\n", " ", "", "\n  ", "\r\n", ";", ","}
	var units []string
	for _, key := range keys {
		for _, opener := range openers {
			for _, end := range ends {
				units = append(units, key+opener+end)
			}
		}
	}
	pairKeys := []string{`"token":`, `x_token=`, `Cookie: `, `'Authorization: `}
	pairOpeners := []string{`[`, `["`, `[\"`, `['`, `"`, `'`, `\"`, `[[`, `{`, `"""`, `["a"`, `]`, `"]`, `']`}
	for _, first := range pairKeys {
		for _, firstOpener := range pairOpeners {
			for _, second := range pairKeys {
				for _, secondOpener := range pairOpeners {
					for _, end := range []string{"\n", " "} {
						units = append(units, first+firstOpener+end+second+secondOpener+end)
					}
				}
			}
		}
	}
	sizes := []int{4 << 10, 16 << 10, 64 << 10}
	if testing.Short() {
		// A random sample of one unit in 128, at 1 KiB and 4 KiB: a quadratic
		// shape already overdraws the bound at 4 KiB, and the race detector
		// multiplies the cost of the scan many times. The seed changes from
		// run to run, so the runs cover different units; it is logged to
		// repeat a failing run.
		seed := time.Now().UnixNano()
		t.Logf("short mode: sampling units with seed %d", seed)
		sample := rand.New(rand.NewSource(seed)).Perm(len(units))[:len(units)/128]
		picked := make([]string, 0, len(sample))
		for _, index := range sample {
			picked = append(picked, units[index])
		}
		units, sizes = picked, []int{1 << 10, 4 << 10}
	}
	const perByte = 48
	const shards = 8
	for shard := range shards {
		t.Run(fmt.Sprintf("shard%d", shard), func(t *testing.T) {
			t.Parallel()
			worst, worstUnit, failures := 0.0, "", 0
			for index := shard; index < len(units); index += shards {
				unit := units[index]
				for _, size := range sizes {
					text := strings.Repeat(unit, size/len(unit))
					read := 0
					scanSecrets(text, "<REDACTED:secret>", &read)
					if ratio := float64(read) / float64(len(text)); ratio > worst {
						worst, worstUnit = ratio, unit
					}
					if read > perByte*len(text) {
						t.Errorf("%q: the scanner examines %d bytes of %d bytes of text", unit, read, len(text))
						if failures++; failures == 10 {
							t.Fatal("stopping after 10 units")
						}
						break
					}
				}
			}
			t.Logf("at most %.1f bytes examined per byte of text, for %q", worst, worstUnit)
		})
	}
}
