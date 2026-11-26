package redaction

import (
	"fmt"
	"regexp"
	"strings"
)

type Redactor struct {
	rules []redactRule
}

type redactRule struct {
	name string
	re   *regexp.Regexp
	repl string
	// need is a cheap test that every match of the rule passes. When it
	// fails the rule cannot match, so running it is skipped. A nil need
	// (user-supplied patterns) always runs the expression.
	need *condition
	// scan marks the rule that replaces the values of key/value pairs with
	// scanSecrets instead of an expression; re is nil for it. The first
	// group of its need lists the key words it looks for.
	scan bool
	// read, when set, counts the bytes scanSecrets examines. Tests set it
	// to bound the work.
	read *int
}

// condition is a necessary condition for a rule to match: every group must
// find at least one of its literals in the value.
type condition struct {
	groups []literalGroup
}

// literalGroup holds alternative literals. With fold set the literals are
// lower-case and matched the way a (?i) regular expression matches them,
// including the two non-ASCII runes Unicode folds onto ASCII letters
// (U+212A KELVIN SIGN for k, U+017F LATIN SMALL LETTER LONG S for s).
type literalGroup struct {
	literals []string
	fold     bool
	// byStart lists, per byte, the folded literals a match starting with
	// that byte can spell.
	byStart [256][]string
	// With followed set, a folded literal only counts where, after any run
	// of skip bytes, one of the next bytes comes.
	followed   bool
	skip, next [256]bool
}

func exact(literals ...string) literalGroup {
	return literalGroup{literals: literals}
}

func folded(literals ...string) literalGroup {
	group := literalGroup{literals: literals, fold: true}
	for _, literal := range literals {
		for _, variant := range foldVariants(literal[0]) {
			group.byStart[variant[0]] = append(group.byStart[variant[0]], literal)
		}
	}
	return group
}

// followedBy narrows a folded group to the places where a literal is
// followed, after any run of the skip bytes, by one of the next bytes.
func (group literalGroup) followedBy(skip, next string) literalGroup {
	group.followed = true
	for index := range len(skip) {
		group.skip[skip[index]] = true
	}
	for index := range len(next) {
		group.next[next[index]] = true
	}
	return group
}

func needs(groups ...literalGroup) *condition {
	return &condition{groups: groups}
}

func (need *condition) holds(value string) bool {
	if need == nil {
		return true
	}
	for index := range need.groups {
		if !need.groups[index].found(value) {
			return false
		}
	}
	return true
}

func (group *literalGroup) found(value string) bool {
	if !group.fold {
		for _, literal := range group.literals {
			if strings.Contains(value, literal) {
				return true
			}
		}
		return false
	}
	return group.find(value, 0) >= 0
}

// find returns the first position at or after from where a folded literal
// stands (and is followed as the group requires), or -1.
func (group *literalGroup) find(value string, from int) int {
	for start := from; start < len(value); start++ {
		for _, literal := range group.byStart[value[start]] {
			length := foldedPrefix(value[start:], literal)
			if length < 0 {
				continue
			}
			if !group.followed {
				return start
			}
			after := start + length
			for after < len(value) && group.skip[value[after]] {
				after++
			}
			if after < len(value) && group.next[value[after]] {
				return start
			}
		}
	}
	return -1
}

// foldedPrefix returns how many bytes of value spell literal under simple
// case folding, or -1 when value does not start with it. literal is
// lower-case ASCII. It accepts exactly the spellings foldVariants lists for
// each byte.
func foldedPrefix(value, literal string) int {
	position := 0
	for index := 0; index < len(literal); index++ {
		if position >= len(value) {
			return -1
		}
		want, got := literal[index], value[position]
		switch {
		case got == want, want >= 'a' && want <= 'z' && got == want-'a'+'A':
			position++
		case want == 'k' && strings.HasPrefix(value[position:], kelvinSign):
			position += len(kelvinSign)
		case want == 's' && strings.HasPrefix(value[position:], longS):
			position += len(longS)
		default:
			return -1
		}
	}
	return position
}

const (
	kelvinSign = "\u212a" // folds onto k
	longS      = "\u017f" // folds onto s
)

// foldVariants lists every encoding a (?i) regular expression accepts for the
// ASCII byte b: the byte itself, its other ASCII case, and for k and s the
// non-ASCII rune in the same Unicode case-folding orbit.
func foldVariants(b byte) []string {
	switch {
	case b == 'k':
		return []string{"k", "K", kelvinSign}
	case b == 's':
		return []string{"s", "S", longS}
	case b >= 'a' && b <= 'z':
		return []string{string(b), string(b - 'a' + 'A')}
	default:
		return []string{string(b)}
	}
}

// secretKeyWords are the words a key name ends with when its value is a
// secret: password, passwd, pwd, passphrase, secret, token, credential(s),
// the api, access, secret, private, signing, encryption and ssh keys
// (api_key, api-key, apikey, ...), secret_key_base and secret_string
// (SecretString). Anything may come before the word (db_password,
// aws_secret_access_key, x-api-key, clientSecret); nothing may come after
// it, so token_count, max_tokens, secret_name and password_changed_at are
// not secret keys.
//
// What this leaves alone, on purpose:
//
//   - Plural names (tokens, passwords, secrets, api_keys): they are counts
//     and lists of objects far more often than secrets. credentials is the
//     exception.
//   - An object under a secret name, in ScrubFields and ScrubJSON:
//     `"secret": {"name": "db-creds"}` is walked field by field, so a string
//     under another name inside it (`"credentials": {"user": "bob", "pass":
//     "x"}`) is kept. In text the same holds for an object whose brace ends
//     the key's line; one written on the key's line is replaced with the
//     rest of that line.
//   - A name with a suffix after the word: password1, secret_value,
//     token_value.
const secretKeyWords = `passw(?:or)?d|pwd|passphrase|secret|token|credentials?` +
	`|(?:api|access|secret|private|signing|encryption|ssh)[_\-]?key` +
	`|secret_key_base|secret-key-base|secretkeybase|secret[_\-]?string`

// headerKeyWords are the header names whose whole value is a credential.
// Set-Cookie and Proxy-Authorization end with them.
const headerKeyWords = `authorization|cookie`

// privateKeyPattern matches a PEM private key block from its BEGIN line to
// its END line, or to the end of the value when the END line is missing.
const privateKeyPattern = `-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----|$)`

// Each built-in rule carries a necessary condition derived from its
// expression: a literal every match contains, matched case-insensitively
// when the expression is. The condition only decides whether the expression
// runs; the expressions, their order and their replacements decide the output.
//
// The bearer and password rules replace the key together with the value. The
// secret rule (scanSecrets) covers the forms those two miss (quoted keys,
// longer key names, quoted values, other authorization schemes) and keeps
// the key and the quoting, so a line keeps its shape.
//
// The order matters where one rule could take another rule's key as its
// value. A private key block goes first, whole, before the secret rule could
// take its BEGIN line as a bare value. The secret rule runs before the bearer
// and password rules: on a pair both match, the older rule then replaces the
// whole pair as it always did, and a secret can no longer be left behind
// because the older rule consumed its key.
var defaultRules = []struct {
	name, pattern, replacement string
	need                       *condition
	scan                       bool
}{
	{"jwt", `eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`, "", needs(exact("eyJ")), false},
	{"aws_key", `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, "", needs(exact("AKIA", "ASIA")), false},
	{"openai_key", `\bsk-[A-Za-z0-9_\-]{20,}`, "", needs(exact("sk-")), false},
	{"slack_token", `\bxox[abcdeprs]-[A-Za-z0-9\-]{10,}`, "", needs(exact("xox")), false},
	{"basic_auth", `(://)[^/\s:@]+:[^/\s@]+(@)`, `${1}<REDACTED:basic_auth>${2}`, needs(exact("://"), exact("@")), false},
	{"private_key", privateKeyPattern, "", needs(exact("-----BEGIN ")), false},
	{"secret", "", "<REDACTED:secret>", secretNeed, true},
	{"bearer", `(?i)Authorization:\s*Bearer\s+[A-Za-z0-9._\-]+`, "", needs(folded("authorization:"), folded("bearer")), false},
	// The bearer rule stops at the first character outside its token
	// alphabet; this removes the rest of a base64 or percent-encoded token.
	{"bearer_rest", `(<REDACTED:bearer>)[~+/=%][A-Za-z0-9._~+/=%\-]*`, `${1}`, needs(exact("<REDACTED:bearer>")), false},
	{"password", `(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key)\s*[=:]\s*\S+`, "", needs(folded("passw", "pwd", "secret", "token", "api"), exact("=", ":")), false},
	// The basic_auth rule needs a user name; this covers a password alone,
	// as in redis://:password@host. It runs after the password rule, whose
	// match may start inside such a password and end after it.
	// The host after the @ is kept, as basic_auth keeps it.
	{"basic_auth_password", `(://:)[^/\s@]+(@)`, `${1}<REDACTED:basic_auth>${2}`, needs(exact("://:"), exact("@")), false},
	{"email", `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, "", needs(exact("@")), false},
	{"uuid", `\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`, "", needs(exact("-")), false},
	{"user_agent", `Mozilla/[0-9.]+\s*\([^)]*\)(?:[^"\n]*?(?:Gecko|Chrome|Safari|Firefox|Edg|Trident|OPR|MSIE)[^"\n]*)?`, "", needs(exact("Mozilla/")), false},
}

// secretNeed lists every key word the secret rule looks for (and name and
// key, for a name/value pair), followed by what may come before the
// separator, then the separator.
var secretNeed = needs(folded(
	"password", "passwd", "pwd", "passphrase", "secret", "token", "credential", "credentials",
	"api_key", "api-key", "apikey", "access_key", "access-key", "accesskey",
	"secret_key", "secret-key", "secretkey", "private_key", "private-key", "privatekey",
	"signing_key", "signing-key", "signingkey", "encryption_key", "encryption-key", "encryptionkey",
	"ssh_key", "ssh-key", "sshkey", "secret_key_base", "secret-key-base", "secretkeybase",
	"secret_string", "secret-string", "secretstring",
	"authorization", "cookie", "name", "key",
).followedBy("\\\"' \t\n\r\f\v", ":="))

// Values under a field whose name ends with a secret key word or with the
// name of a credential header are secrets whatever they look like, so
// ScrubFields replaces them without looking at the value.
var secretFieldKey = regexp.MustCompile(`(?i)(?:` + secretKeyWords + `|` + headerKeyWords + `)$`)

// headerFieldKey matches a field name that ends with a credential header
// name, so its value is read as a header value.
var headerFieldKey = regexp.MustCompile(`(?i)(?:` + headerKeyWords + `)$`)

var ipRules = []struct {
	name, pattern string
	need          *condition
}{
	{"ipv4", `\b(?:\d{1,3}\.){3}\d{1,3}\b`, needs(exact("."))},
	{"ipv6", `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`, needs(exact(":"))},
}

func NewRedactor(redactIPs bool, extra []string) (*Redactor, []error) {
	redactor := &Redactor{}
	var errs []error
	add := func(name, pattern, replacement string, need *condition, scan bool) {
		rule := redactRule{name: name, repl: replacement, need: need, scan: scan}
		if !scan {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				errs = append(errs, fmt.Errorf("redactor %q: %w", name, err))
				return
			}
			rule.re = compiled
		}
		redactor.rules = append(redactor.rules, rule)
	}
	for _, rule := range defaultRules {
		add(rule.name, rule.pattern, rule.replacement, rule.need, rule.scan)
	}
	if redactIPs {
		for _, rule := range ipRules {
			add(rule.name, rule.pattern, "", rule.need, false)
		}
	}
	for index, pattern := range extra {
		add(fmt.Sprintf("custom%d", index), pattern, "", nil, false)
	}
	return redactor, errs
}

func (redactor *Redactor) Scrub(value string) string {
	if redactor == nil || value == "" {
		return value
	}
	for _, rule := range redactor.rules {
		if !rule.need.holds(value) {
			continue
		}
		replacement := rule.repl
		if replacement == "" {
			replacement = "<REDACTED:" + rule.name + ">"
		}
		if rule.scan {
			value = scanSecrets(value, replacement, rule.read)
		} else {
			value = rule.re.ReplaceAllString(value, replacement)
		}
	}
	return value
}

// ScrubFields returns a copy of fields with every string scrubbed, at any
// depth. The value of a field whose name ends with a secret key word
// (password, api_key, Authorization, ...) is replaced whole; booleans and
// nulls are kept, and a map under such a name is walked field by field.
func (redactor *Redactor) ScrubFields(fields map[string]interface{}) map[string]interface{} {
	if redactor == nil || fields == nil {
		return fields
	}
	clean, _ := fieldWalk{scrub: redactor.Scrub}.fields(fields)
	return clean
}

// ScrubJSON returns a copy of a decoded JSON value (strings, numbers,
// booleans, nil, []interface{} and map[string]interface{}) with every string
// passed through scrub, field names included, and reports whether anything
// changed. As in ScrubFields, the value of a field whose name ends with a
// secret key word is replaced whole, whatever scrub would make of it: a
// string alone under the name password carries nothing a rule can match.
func ScrubJSON(value interface{}, scrub func(string) string) (interface{}, bool) {
	return fieldWalk{scrub: scrub, names: true}.value(value)
}

// ScrubField returns a copy of the value held under a field called name,
// with every string in it passed through scrub. When name ends with a secret
// key word the value is replaced as in ScrubFields. Field names inside the
// value are judged the same way and are not themselves scrubbed.
func ScrubField(name string, value interface{}, scrub func(string) string) interface{} {
	walk := fieldWalk{scrub: scrub}
	var clean interface{}
	if secretFieldKey.MatchString(name) {
		clean, _ = walk.secret(value)
	} else {
		clean, _ = walk.value(value)
	}
	return clean
}

// ScrubLabel is ScrubField for a string held under a label or field name, as
// in a map[string]string: the scrubbed value, or the secret marker when the
// name ends with a secret key word.
func ScrubLabel(name, value string, scrub func(string) string) string {
	return ScrubField(name, value, scrub).(string)
}

// ScrubFieldIn is ScrubField for a value taken from fields: it is also
// replaced when it is the value field of a map that names a secret in its
// name or key field, as a Kubernetes env entry or an AWS tag does.
func ScrubFieldIn[V any](fields map[string]V, name string, value interface{}, scrub func(string) string) interface{} {
	if SecretFieldIn(fields, name) {
		clean, _ := fieldWalk{scrub: scrub}.secret(value)
		return clean
	}
	return ScrubField(name, value, scrub)
}

// ScrubLabelIn is ScrubFieldIn for a string, as in a map[string]string.
func ScrubLabelIn[V any](fields map[string]V, name, value string, scrub func(string) string) string {
	return ScrubFieldIn(fields, name, value, scrub).(string)
}

// SecretFieldIn reports whether the value held under name in fields is a
// secret: name ends with a secret key word or with authorization, or fields
// names a secret in its name or key field and name is its value field.
func SecretFieldIn[V any](fields map[string]V, name string) bool {
	return secretField(name, namesSecret(fields))
}

// namesSecret reports whether a map names a secret in its name or key field
// (a Kubernetes env entry, an AWS tag, a header in a list of headers). Such
// a map holds the secret in its value field.
func namesSecret[V any](fields map[string]V) bool {
	for _, nameField := range [...]string{"name", "Name", "key", "Key"} {
		if value, found := fields[nameField]; found {
			if name, ok := any(value).(string); ok && secretFieldKey.MatchString(name) {
				return true
			}
		}
	}
	return false
}

// secretField reports whether the value under key is a secret, given
// whether the map it sits in names one.
func secretField(key string, named bool) bool {
	return secretFieldKey.MatchString(key) || (named && (key == "value" || key == "Value"))
}

const secretFieldMarker = "<REDACTED:secret>"

// fieldWalk copies structured data, scrubbing as it goes. names says whether
// field names are scrubbed too.
type fieldWalk struct {
	scrub func(string) string
	names bool
}

func (walk fieldWalk) fields(fields map[string]interface{}) (map[string]interface{}, bool) {
	if fields == nil {
		return fields, false
	}
	named := namesSecret(fields)
	clean := make(map[string]interface{}, len(fields))
	changed := false
	for key, value := range fields {
		var valueChanged bool
		if secretField(key, named) {
			value, valueChanged = walk.secret(value)
		} else {
			value, valueChanged = walk.value(value)
		}
		changed = changed || valueChanged
		if walk.names {
			if cleanKey := walk.scrub(key); cleanKey != key {
				key, changed = cleanKey, true
			}
		}
		clean[key] = value
	}
	return clean, changed
}

func (walk fieldWalk) value(value interface{}) (interface{}, bool) {
	switch typed := value.(type) {
	case string:
		clean := walk.scrub(typed)
		return clean, clean != typed
	case []interface{}:
		clean := make([]interface{}, len(typed))
		changed := false
		for index, item := range typed {
			var itemChanged bool
			clean[index], itemChanged = walk.value(item)
			changed = changed || itemChanged
		}
		return clean, changed
	case map[string]interface{}:
		return walk.fields(typed)
	default:
		return value, false
	}
}

// secret replaces a value held under a secret field name. Booleans and nil
// are kept; a list is replaced item by item and a map is walked as fields.
func (walk fieldWalk) secret(value interface{}) (interface{}, bool) {
	switch typed := value.(type) {
	case nil, bool:
		return value, false
	case []interface{}:
		clean := make([]interface{}, len(typed))
		changed := false
		for index, item := range typed {
			var itemChanged bool
			clean[index], itemChanged = walk.secret(item)
			changed = changed || itemChanged
		}
		return clean, changed
	case map[string]interface{}:
		return walk.fields(typed)
	case string:
		return secretFieldMarker, typed != secretFieldMarker
	default:
		return secretFieldMarker, true
	}
}
