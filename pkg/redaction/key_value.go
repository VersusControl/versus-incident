package redaction

import (
	"sort"
	"strings"
)

// scanSecrets replaces the values of secret key/value pairs in text with
// marker and keeps the rest of each pair: the key, its separator and the
// quotes or brackets around the value. It reads the text once, from left to
// right, so its work grows with the length of the text whatever the text
// holds.
//
// A key is a secret key word (secretKeyWords), the name of a credential
// header (authorization, cookie), or name/key for a name/value pair, written
// in any case and with the non-ASCII letters Unicode folds onto k and s.
// Anything may come before the word (db_password, aws_secret_access_key,
// x-api-key); nothing may come after it but the end of the key: an optional
// quote (backslash-escaped inside a JSON string), whitespace that may hold a
// line break, and one or more : or = with an optional > (:=, =>).
//
// What follows the separator is read as the first of these that fits:
//
//	a. a string in backslash-escaped double quotes (a pair inside a JSON
//	   string), up to the escaped closing quote;
//	b. a string in triple quotes, which may span lines;
//	c. a double-quoted string, with backslash escapes;
//	d. a single-quoted string, with backslash escapes and doubled quotes;
//	   for c and d, text glued to the closing quote ("v"x) goes with the
//	   value, as the password rule's \S+ takes it;
//	e. an array, up to its closing bracket, which may span lines, with
//	   strings in it read as strings and arrays in it at any depth; text
//	   glued to the closing bracket goes with the value;
//	f. a bare true, false or null, which is not a secret and is kept;
//	g. the lines after the key, when the key's line ends after the
//	   separator, a YAML block indicator (| or >) or a comment; see
//	   linesValue;
//	h. an opening brace at the end of the line: an object, which is kept
//	   and whose fields on the next lines are matched by their own names;
//	i. a value with a scheme prefix in braces ({bcrypt}..., {SSHA}...), up
//	   to whitespace;
//	j. any other value that starts with a brace, with the rest of its line:
//	   an object written on one line cannot be told from a secret that
//	   starts with a brace, and replacing only its first word would leave a
//	   marker next to what follows;
//	k. any other bare value, up to whitespace. This takes the rest of an
//	   unspaced JSON or YAML flow map with it: a bare secret may hold any
//	   punctuation, so nothing but whitespace ends it.
//
// The quoted forms (a to e) may start on the line after the key. A
// header key's value is a quoted form, the lines after the key, or else
// everything up to the end of the line, whatever quotes it holds (`Cookie:
// a="x"; sid=y`). A scheme word (Bearer, Basic, Token) at its start is kept.
// When a header written as text sits in a quoted string (`curl -H 'Cookie:
// ...'`, `"Authorization: ApiKey ..."`), its value ends at the quote that
// can close that string. `Authorization: Bearer <token>` is left to the
// bearer rule, which replaces it whole as it always did.
//
// A name/value pair (`"name":"DB_PASSWORD","value":"..."`, as in a
// Kubernetes env entry, an AWS tag or a list of headers) is a name or key
// field whose value ends with a secret key word or a header name, followed
// by a value field: the value field's value is read as above, as a header
// key's value when the name is a header name.
//
// A value can hold the key of another pair: `secret: db_password: hunter2`,
// `token= api_key="a b"`, or `token: " password="hunter2"` where the first
// quote closes an earlier string. The scan keeps looking for keys inside
// values and follows such a pair's value too, so its end is known; the
// value of a pair whose key sits in another value reaches at least over the
// word after its separator, which the password rule would have taken; a key
// in that word reaches over the word after its own separator too. The
// replaced parts are the union of all values; parts that overlap or touch
// are replaced by one marker.
//
// A double- or single-quoted string that meets a line break before its
// closing quote ends there and the scan goes on with the next line: a JSON
// string never holds a raw line break, and an unterminated quote must not
// hide the lines after it. A string in an array ends at a line break too,
// and the array goes on. An array and a triple-quoted string may span lines;
// when the text ends inside one, the value runs to the end. An array that
// holds text on its opening line (wrapped, as pprint and util.inspect print
// a long list) is followed over at most maxWrappedArrayLines line breaks,
// and then runs to the end. When more than maxOpenValues values are open at
// once, as only text built to nest pairs can make them, one that ends on the
// current line is no longer followed: the text from where it starts to the
// end of the line is replaced. When all of them can span lines, the text
// from the first of them to the end is replaced.
//
// Not covered: a secret after a flag with no separator (--password hunter2,
// "args":["--password","hunter2"]), a name/value pair with the value first
// or another field between the two, and the body of a PEM block that
// arrives one line per text (the private key rule needs the BEGIN line in
// the same text).
//
// read, when not nil, is increased by the number of bytes the scan
// examined; tests use it to bound the work.
func scanSecrets(text, marker string, read *int) string {
	scan := kvScanner{text: text, keys: &secretNeed.groups[0]}
	scan.run()
	if read != nil {
		*read += scan.work
	}
	if len(scan.spans) == 0 {
		return text
	}
	sort.SliceStable(scan.spans, func(i, j int) bool { return scan.spans[i].start < scan.spans[j].start })
	var out strings.Builder
	out.Grow(len(text))
	copied := 0
	for index := 0; index < len(scan.spans); {
		start, end := scan.spans[index].start, scan.spans[index].end
		for index++; index < len(scan.spans) && scan.spans[index].start <= end; index++ {
			end = max(end, scan.spans[index].end)
		}
		out.WriteString(text[copied:start])
		out.WriteString(marker)
		copied = end
	}
	out.WriteString(text[copied:])
	return out.String()
}

// maxOpenValues is how many values the scan follows at once. Ordinary text
// has one, or a few when a value holds pairs of its own.
const maxOpenValues = 16

// maxWrappedArrayLines is how many line breaks an array that holds text on
// its opening line (wrapped, as Python's pprint and Node's util.inspect
// print a long list) is followed over. Past it the value runs to the end of
// the text.
const maxWrappedArrayLines = 256

type span struct{ start, end int }

type kvScanner struct {
	text  string
	keys  *literalGroup
	open  []*valueParser
	spans []span
	work  int
	// quiet is where key search resumes: no key starts before it.
	quiet int
	// lineEnd is where the line ends that evict last looked at.
	lineEnd int
	// indent is the indentation of the current line so far, and leading
	// says whether only indentation has been read on it.
	indent  int
	leading bool
	// evicted is where the line text evict last replaced ends.
	evicted int
	// stopped is set when the rest of the text is replaced.
	stopped bool
}

func (scan *kvScanner) run() {
	text := scan.text
	for pos := 0; pos <= len(text); pos++ {
		eot := pos == len(text)
		var c byte
		if !eot {
			c = text[pos]
		}
		kept := scan.open[:0]
		for _, parser := range scan.open {
			if parser.start <= pos {
				scan.work++
				parser.feed(scan, pos, c, eot)
			}
			if parser.finished() {
				scan.close(parser)
			} else {
				kept = append(kept, parser)
			}
		}
		scan.open = kept
		if eot || scan.stopped {
			break
		}
		if pos == 0 || text[pos-1] == '\n' {
			scan.indent, scan.leading = 0, true
		}
		if scan.leading {
			if c == ' ' || c == '\t' {
				scan.indent++
			} else {
				scan.leading = false
			}
		}
		if pos >= scan.quiet {
			scan.detect(pos)
		}
	}
}

// close records the span a finished parser replaces.
func (scan *kvScanner) close(parser *valueParser) {
	start, end := parser.valueStart, parser.valueEnd
	if parser.swallowed {
		if end > start {
			end = max(end, parser.wordEnd)
		} else {
			start, end = parser.wordStart, parser.wordEnd
		}
	} else if parser.kept {
		return
	}
	if end > start {
		scan.spans = append(scan.spans, span{start, end})
	}
}

// detect looks for a key at pos and, when one stands there, starts
// following its value.
func (scan *kvScanner) detect(pos int) {
	text := scan.text
	for _, literal := range scan.keys.byStart[text[pos]] {
		length := foldedPrefix(text[pos:], literal)
		scan.work += len(literal)
		if length < 0 {
			continue
		}
		end, quotedKey := scan.keyEnd(pos + length)
		if end < 0 {
			continue
		}
		kind := kindSecret
		switch literal {
		case "authorization", "cookie":
			kind = kindHeader
		case "name", "key":
			kind = kindName
		}
		swallowed := scan.inValue(pos)
		if kind == kindHeader && literal == "authorization" && text[pos+length] == ':' && !swallowed {
			if resume := scan.bearerToken(pos + length + 1); resume >= 0 {
				// Left to the bearer rule.
				scan.quiet = resume
				return
			}
		}
		named := kind == kindName
		if named {
			var header bool
			if end, header = scan.namedValue(end); end < 0 {
				continue
			}
			kind = kindSecret
			if header {
				kind = kindHeader
			}
		}
		parser := &valueParser{kind: kind, start: end, keyIndent: scan.indent, swallowed: swallowed, wordFrom: -1, regionStart: -1}
		if kind == kindHeader && !quotedKey && !named {
			parser.enclosure, parser.escapedEnclosure = scan.enclosure(pos)
		}
		scan.quiet = end
		scan.open = append(scan.open, parser)
		if len(scan.open) > maxOpenValues {
			scan.evict(pos)
		}
		return
	}
}

// inValue reports whether pos lies in a value an open parser follows, in
// the word a swallowed key reaches over, or in the rest of a line evict
// replaced: a key there is swallowed too, so it reaches over its own next
// word, as the password rule would.
func (scan *kvScanner) inValue(pos int) bool {
	if pos < scan.evicted {
		return true
	}
	for _, parser := range scan.open {
		scan.work++
		if !parser.valueDone && parser.regionStart >= 0 && parser.regionStart <= pos {
			return true
		}
		if parser.swallowed && parser.wordState == 2 && parser.wordStart <= pos {
			return true
		}
	}
	return false
}

// evict stops following one value, so that no more than maxOpenValues are
// followed at once. It picks the value opened first among those that end on
// the line at pos, and replaces the text from where that value starts to the
// end of the line; the other values, the last key on the line included, are
// still followed. When every open value can go on past the line, none can be
// dropped without leaving its later lines in clear: the text from the
// earliest of them to the end is replaced and the scan stops.
func (scan *kvScanner) evict(pos int) {
	victim := -1
	for index, parser := range scan.open {
		scan.work++
		if parser.endsOnLine() {
			victim = index
			break
		}
	}
	if victim < 0 {
		from := len(scan.text)
		for _, parser := range scan.open {
			from = min(from, parser.regionFrom())
		}
		scan.spans = append(scan.spans, span{from, len(scan.text)})
		scan.open = scan.open[:0]
		scan.stopped = true
		return
	}
	if pos >= scan.lineEnd {
		scan.lineEnd = len(scan.text)
		if index := strings.IndexByte(scan.text[pos:], '\n'); index >= 0 {
			scan.lineEnd = pos + index
		}
		scan.work += scan.lineEnd - pos
	}
	scan.spans = append(scan.spans, span{scan.open[victim].regionFrom(), scan.lineEnd})
	scan.evicted = scan.lineEnd
	scan.open = append(scan.open[:victim], scan.open[victim+1:]...)
}

// regionFrom returns where the value, as far as it is known, starts.
func (parser *valueParser) regionFrom() int {
	if parser.regionStart >= 0 {
		return parser.regionStart
	}
	return parser.start
}

// endsOnLine reports whether what the parser still follows ends on the
// current line: a value that can no longer span lines and, for a swallowed
// key, a word that has started.
func (parser *valueParser) endsOnLine() bool {
	if parser.swallowed && parser.wordState < 2 {
		return false
	}
	if parser.valueDone {
		return true
	}
	switch parser.mode {
	case modeLead, modeBackslashes, modeDoubleOpen, modeDoubleTwo, modeSingleOpen, modeSingleTwo, modeTriple,
		modeArray, modeArrayDouble, modeArrayDoubleEscape, modeArraySingle, modeIndicator, modeComment, modeLines:
		return false
	}
	return true
}

// keyEnd reads what may follow a key word: backslashes, a quote,
// whitespace, then one or more : or = and an optional >. It returns where
// that ends, or -1, and whether it held a quote.
func (scan *kvScanner) keyEnd(at int) (int, bool) {
	text := scan.text
	from := at
	for at < len(text) && text[at] == '\\' {
		at++
	}
	quoted := at < len(text) && (text[at] == '"' || text[at] == '\'')
	if quoted {
		at++
	}
	for at < len(text) && isSpace(text[at]) {
		at++
	}
	separator := at
	for at < len(text) && (text[at] == ':' || text[at] == '=') {
		at++
	}
	scan.work += at - from + 1
	if at == separator {
		return -1, false
	}
	if at < len(text) && text[at] == '>' {
		at++
	}
	return at, quoted
}

// bearerToken reports whether `Authorization:` at at-1 is followed by
// whitespace, the word bearer, whitespace and a token character, and
// returns where the token starts, or -1.
func (scan *kvScanner) bearerToken(at int) int {
	text := scan.text
	from := at
	defer func() { scan.work += at - from + 1 }()
	for at < len(text) && isSpace(text[at]) {
		at++
	}
	length := foldedPrefix(text[at:], "bearer")
	if length < 0 {
		return -1
	}
	at += length
	spaces := at
	for at < len(text) && isSpace(text[at]) {
		at++
	}
	if at == spaces || at == len(text) {
		return -1
	}
	if c := text[at]; c == '.' || c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		strings.HasPrefix(text[at:], kelvinSign) || strings.HasPrefix(text[at:], longS) {
		return at
	}
	return -1
}

// namedValue reads the rest of a name/value pair after `name:` or `key:`:
// a name that ends with a secret key word or a header name, then a value
// field. It returns where the value field's separator ends, or -1, and
// whether the name is a header name, whose value is then read as a header
// value (`- name: Authorization` / `value: Bearer ...`).
func (scan *kvScanner) namedValue(at int) (int, bool) {
	text := scan.text
	from := at
	defer func() { scan.work += at - from + 1 }()
	skip := func(set func(byte) bool) {
		for at < len(text) && set(text[at]) {
			at++
		}
	}
	backslash := func(c byte) bool { return c == '\\' }
	quote := func() {
		if at < len(text) && (text[at] == '"' || text[at] == '\'') {
			at++
		}
	}
	skip(isSpace)
	skip(backslash)
	quote()
	name := at
	for at < len(text) {
		c := text[at]
		switch {
		case c == '_' || c == '.' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			at++
			continue
		case strings.HasPrefix(text[at:], kelvinSign):
			at += len(kelvinSign)
			continue
		case strings.HasPrefix(text[at:], longS):
			at += len(longS)
			continue
		}
		break
	}
	if at == name || !secretFieldKey.MatchString(text[name:at]) {
		return -1, false
	}
	header := headerFieldKey.MatchString(text[name:at])
	scan.work += at - name
	skip(backslash)
	quote()
	skip(isSpace)
	if at < len(text) && text[at] == ',' {
		at++
	}
	skip(isSpace)
	skip(backslash)
	quote()
	length := foldedPrefix(text[at:], "value")
	if length < 0 {
		return -1, false
	}
	at += length
	end, _ := scan.keyEnd(at)
	return end, header
}

// enclosure reports the quote a header written as text sits in: the quote
// right before the header name, and whether it is backslash-escaped. It is
// 0 when there is none.
func (scan *kvScanner) enclosure(keyStart int) (byte, bool) {
	text := scan.text
	nameStart := keyStart
	for nameStart > 0 && headerNameByte(text[nameStart-1]) {
		nameStart--
	}
	scan.work += keyStart - nameStart + 1
	if nameStart == 0 {
		return 0, false
	}
	quote := text[nameStart-1]
	if quote != '"' && quote != '\'' {
		return 0, false
	}
	return quote, quote == '"' && nameStart >= 2 && text[nameStart-2] == '\\'
}

// headerNameByte reports whether b can be part of a header name.
func headerNameByte(b byte) bool {
	return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// closesString reports whether b, right after a quote, shows that the quote
// closes a string.
func closesString(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', ')', ']', '}', '"', '\\':
		return true
	}
	return false
}

// isSpace reports whether b is whitespace as \s means it in a regular
// expression: space, tab, line feed, form feed or carriage return.
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\f' || b == '\r'
}

// schemeByte reports whether b may be part of a {scheme} prefix.
func schemeByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

type keyKind int

const (
	kindSecret keyKind = iota
	kindHeader
	kindName
)

type valueMode int

const (
	modeLead              valueMode = iota // after the separator
	modeBackslashes                        // backslashes at the start of the value
	modeDoubleOpen                         // " read: a string, or "" or """
	modeDoubleTwo                          // "" read
	modeSingleOpen                         // ' read
	modeSingleTwo                          // '' read
	modeDouble                             // in "..."
	modeDoubleEscape                       // after \ in "..."
	modeSingle                             // in '...'
	modeSingleEscape                       // after \ in '...'
	modeSingleQuote                        // ' read in '...': doubled or closing
	modeTriple                             // in """...""" or '''...'''
	modeEscaped                            // in \"...\"
	modeArray                              // in [...]
	modeArrayDouble                        // in "..." in an array
	modeArrayDoubleEscape                  // after \ in "..." in an array
	modeArraySingle                        // in '...' in an array
	modeAfterClose                         // after a closing quote or bracket
	modeGlued                              // text glued to it
	modeBare                               // to whitespace
	modeRestOfLine                         // to the end of the line
	modeHeader                             // a header written as text
	modeHeaderQuote                        // a quote read in it
	modeBrace                              // { read
	modeBraceScheme                        // in {scheme
	modeBraceAfter                         // {scheme} read
	modeIndicator                          // after | or >
	modeComment                            // in a # comment on the key's line
	modeLines                              // the lines after the key
)

// valueParser follows the value of one pair, one byte at a time.
type valueParser struct {
	kind             keyKind
	start            int // the first position it reads
	keyIndent        int
	swallowed        bool // the key sits in another value
	enclosure        byte // a header in a quoted string: that quote
	escapedEnclosure bool

	mode valueMode
	// regionStart is where the value, as far as it is known, starts, or -1
	// before that.
	regionStart          int
	valueStart, valueEnd int
	valueDone, kept      bool

	sawNewline bool // in modeLead
	lineIndent int  // indentation of the current line, in modeLead
	from       int  // first byte of the value form (bare, brace, header)
	firstSpace int  // first whitespace after from, or -1
	quote      byte
	run        int // backslashes or quotes in a row; line breaks in a wrapped array
	closeAt    int
	glued      string // bytes that end a value at its closing quote or bracket
	depth      int
	mark       int // a position the mode remembers
	wrapped    bool

	// The lines after the key; see linesValue.
	block                     bool
	commentStart, commentEnd  int
	linesStart, linesEnd      int
	bodyStarted, item, dashed bool
	bodyStart, lastNonSpace   int
	mapping, colon            bool

	// The word after the separator, for a swallowed key.
	wordFrom, wordStart, wordEnd int
	wordState                    int
}

func (parser *valueParser) finished() bool {
	return parser.valueDone && (!parser.swallowed || parser.wordState == 3)
}

const stringGlue = ",]})\"';:/<>"
const arrayGlue = ",]})\"';"

func (parser *valueParser) feed(scan *kvScanner, pos int, c byte, eot bool) {
	if !parser.valueDone {
		parser.step(scan, pos, c, eot)
	}
	if !parser.swallowed || parser.wordState == 3 {
		return
	}
	if eot && parser.wordFrom < 0 {
		parser.wordFrom = pos
	}
	if parser.wordFrom >= 0 && pos >= parser.wordFrom {
		parser.trackWord(pos, c, eot)
	}
}

// trackWord follows the first word at or after wordFrom.
func (parser *valueParser) trackWord(pos int, c byte, eot bool) {
	switch {
	case eot && parser.wordState == 2:
		parser.wordEnd, parser.wordState = pos, 3
	case eot:
		parser.wordStart, parser.wordEnd, parser.wordState = pos, pos, 3
	case parser.wordState <= 1 && !isSpace(c):
		parser.wordStart, parser.wordState = pos, 2
	case parser.wordState == 0:
		parser.wordState = 1
	case parser.wordState == 2 && isSpace(c):
		parser.wordEnd, parser.wordState = pos, 3
	}
}

func (parser *valueParser) done(start, end int) {
	parser.valueStart, parser.valueEnd, parser.valueDone = start, end, true
}

func (parser *valueParser) begin(at int) {
	if parser.regionStart < 0 {
		parser.regionStart = at
	}
}

func (parser *valueParser) step(scan *kvScanner, pos int, c byte, eot bool) {
	switch parser.mode {
	case modeLead:
		parser.lead(scan, pos, c, eot)
	case modeBackslashes:
		switch {
		case !eot && c == '\\':
		case !eot && c == '"':
			parser.content(scan, modeEscaped, pos+1)
		case parser.sawNewline:
			// Not an escaped string: the lines after the key, the first of
			// which starts with the backslashes.
			from := parser.mark
			parser.enterLines(pos)
			for at := from; at <= pos; at++ {
				scan.work++
				parser.lines(at, scan.text, at == len(scan.text))
			}
		default:
			parser.keyLine(scan, parser.mark, true)
			parser.step(scan, pos, c, eot)
		}
	case modeDoubleOpen:
		switch {
		case !eot && c == '"':
			parser.mode = modeDoubleTwo
		default:
			parser.content(scan, modeDouble, pos)
			parser.step(scan, pos, c, eot)
		}
	case modeDoubleTwo:
		if !eot && c == '"' {
			parser.quote = '"'
			parser.content(scan, modeTriple, pos+1)
			return
		}
		// "" and then not a third quote: an empty string.
		parser.content(scan, modeDouble, pos-1)
		parser.closeAt, parser.glued, parser.mode = pos-1, stringGlue, modeAfterClose
		parser.step(scan, pos, c, eot)
	case modeSingleOpen:
		switch {
		case !eot && c == '\'':
			parser.mode = modeSingleTwo
		default:
			parser.content(scan, modeSingle, pos)
			parser.step(scan, pos, c, eot)
		}
	case modeSingleTwo:
		if !eot && c == '\'' {
			parser.quote = '\''
			parser.content(scan, modeTriple, pos+1)
			return
		}
		parser.content(scan, modeSingle, pos-1)
		parser.closeAt, parser.glued, parser.mode = pos-1, stringGlue, modeAfterClose
		parser.step(scan, pos, c, eot)
	case modeDouble:
		switch {
		case eot:
			parser.done(parser.valueStart, pos)
		case c == '\\':
			parser.mode = modeDoubleEscape
		case c == '"':
			parser.closeAt, parser.glued, parser.mode = pos, stringGlue, modeAfterClose
		case c == '\n':
			parser.done(parser.valueStart, pos)
		}
	case modeDoubleEscape:
		if eot || c == '\n' {
			parser.done(parser.valueStart, pos)
			return
		}
		parser.mode = modeDouble
	case modeSingle:
		switch {
		case eot:
			parser.done(parser.valueStart, pos)
		case c == '\\':
			parser.mode = modeSingleEscape
		case c == '\'':
			parser.mark, parser.mode = pos, modeSingleQuote
		case c == '\n':
			parser.done(parser.valueStart, pos)
		}
	case modeSingleEscape:
		if eot || c == '\n' {
			parser.done(parser.valueStart, pos)
			return
		}
		parser.mode = modeSingle
	case modeSingleQuote:
		if !eot && c == '\'' {
			parser.mode = modeSingle // a doubled quote
			return
		}
		parser.closeAt, parser.glued, parser.mode = parser.mark, stringGlue, modeAfterClose
		parser.step(scan, pos, c, eot)
	case modeTriple:
		switch {
		case eot:
			parser.done(parser.valueStart, pos)
		case c == parser.quote:
			if parser.run++; parser.run == 3 {
				parser.done(parser.valueStart, pos-2)
			}
		default:
			parser.run = 0
		}
	case modeEscaped:
		switch {
		case eot:
			parser.done(parser.valueStart, pos)
		case c == '\\':
			parser.run++
		case c == '"' && parser.run <= 1:
			parser.done(parser.valueStart, max(parser.valueStart, pos-parser.run))
		case c == '"':
			parser.run = 0 // an escaped backslash and quote, inside the string
		case c == '\n':
			parser.done(parser.valueStart, pos)
		default:
			parser.run = 0
		}
	case modeArray:
		switch {
		case eot:
			parser.done(parser.valueStart, pos)
			return
		case c == '\n':
			// An array goes on over line breaks to its closing bracket.
			// One that holds text on its opening line is wrapped: it is
			// followed over at most maxWrappedArrayLines line breaks, and
			// then runs to the end of the text.
			if parser.mark == 1 {
				parser.wrapped = true
			}
			parser.mark = 2
			if parser.wrapped {
				if parser.run++; parser.run > maxWrappedArrayLines {
					parser.done(parser.valueStart, len(scan.text))
				}
			}
			return
		case parser.mark == 0 && c != ' ' && c != '\t' && c != '\r':
			parser.mark = 1
		}
		switch {
		case c == '[':
			parser.depth++
		case c == ']':
			if parser.depth--; parser.depth == 0 {
				parser.closeAt, parser.glued, parser.mode = pos, arrayGlue, modeAfterClose
			}
		case c == '"':
			parser.mode = modeArrayDouble
		case c == '\'':
			parser.mode = modeArraySingle
		}
	case modeArrayDouble:
		switch {
		case eot, c == '\n':
			// A string in an array ends at a line break; the array goes on.
			parser.mode = modeArray
			parser.step(scan, pos, c, eot)
		case c == '\\':
			parser.mode = modeArrayDoubleEscape
		case c == '"':
			parser.mode = modeArray
		}
	case modeArrayDoubleEscape:
		if eot || c == '\n' {
			parser.mode = modeArray
			parser.step(scan, pos, c, eot)
			return
		}
		parser.mode = modeArrayDouble
	case modeArraySingle:
		switch {
		case eot, c == '\n':
			parser.mode = modeArray
			parser.step(scan, pos, c, eot)
		case c == '\'':
			parser.mode = modeArray
		}
	case modeAfterClose:
		if eot || isSpace(c) || strings.IndexByte(parser.glued, c) >= 0 {
			parser.done(parser.valueStart, parser.closeAt)
			return
		}
		parser.mode = modeGlued
	case modeGlued, modeBare:
		if eot || isSpace(c) {
			parser.done(parser.valueStart, pos)
		}
	case modeRestOfLine:
		if eot || c == '\n' {
			parser.done(parser.valueStart, pos)
		}
	case modeHeader:
		switch {
		case eot, c == '\n':
			parser.done(parser.valueStart, pos)
		case pos >= parser.valueStart && parser.enclosure != 0 && c == parser.enclosure &&
			(pos > 0 && scan.text[pos-1] == '\\') == parser.escapedEnclosure:
			parser.mark, parser.mode = pos, modeHeaderQuote
		}
	case modeHeaderQuote:
		if eot || closesString(c) {
			end := parser.mark
			if parser.escapedEnclosure {
				end--
			}
			parser.done(parser.valueStart, max(parser.valueStart, end))
			return
		}
		parser.mode = modeHeader
		parser.step(scan, pos, c, eot)
	case modeBrace:
		switch {
		case eot:
			parser.done(parser.from, pos)
		case c == ' ' || c == '\t':
			parser.run = 1
		case c == '\r':
			parser.run = 2
		case c == '\n':
			if parser.swallowed {
				parser.done(parser.from, parser.from+1)
				return
			}
			parser.kept, parser.valueDone = true, true // an object; its fields are matched by name
		case parser.run == 0 && schemeByte(c):
			parser.mode = modeBraceScheme
		default:
			parser.mode = modeRestOfLine
		}
	case modeBraceScheme:
		switch {
		case eot, c == '\n':
			parser.done(parser.from, pos)
		case c == '}':
			parser.mode = modeBraceAfter
		case !schemeByte(c):
			parser.mode = modeRestOfLine
		}
	case modeBraceAfter:
		switch {
		case eot, c == '\n':
			parser.done(parser.from, pos)
		case isSpace(c):
			parser.mode = modeRestOfLine
		default:
			parser.mode = modeBare
		}
	case modeIndicator:
		switch {
		case eot:
			parser.fallBack(scan, pos)
		case c == '\n':
			parser.enterLines(pos + 1)
		case c == '#':
			parser.commentStart, parser.mode = pos, modeComment
		case c == '\r':
			parser.noteSpace(pos)
			parser.run = 2
		case parser.run == 2:
			parser.fallBack(scan, pos)
		case c == ' ' || c == '\t':
			parser.noteSpace(pos)
			parser.run = 1
		case parser.run == 0 && (c == '+' || c == '-' || c >= '0' && c <= '9'):
		default:
			parser.fallBack(scan, pos)
		}
	case modeComment:
		switch {
		case eot:
			parser.fallBack(scan, pos)
		case c == '\n':
			parser.commentEnd = pos
			if pos > parser.commentStart && scan.text[pos-1] == '\r' {
				parser.commentEnd--
			}
			parser.enterLines(pos + 1)
		case isSpace(c):
			parser.noteSpace(pos)
		}
	case modeLines:
		parser.lines(pos, scan.text, eot)
	}
}

func (parser *valueParser) noteSpace(pos int) {
	if parser.firstSpace < 0 {
		parser.firstSpace = pos
	}
}

// fallBack ends an indicator or comment on the key's line that is not
// followed by a line break: the value is then read from its first byte as a
// header written as text or as a bare value.
func (parser *valueParser) fallBack(scan *kvScanner, pos int) {
	if parser.kind == kindHeader {
		parser.valueStart, parser.mode = parser.from, modeHeader
		parser.step(scan, pos, scan.byteAt(pos), pos == len(scan.text))
		return
	}
	end := pos
	if parser.firstSpace >= 0 {
		end = parser.firstSpace
	}
	if end < pos || pos == len(scan.text) {
		parser.done(parser.from, end)
		return
	}
	parser.valueStart, parser.mode = parser.from, modeBare
}

// content starts a quoted or bracketed value whose content starts at at.
func (parser *valueParser) content(scan *kvScanner, mode valueMode, at int) {
	parser.mode, parser.run = mode, 0
	parser.valueStart = at
	if parser.kind == kindHeader {
		parser.valueStart = scan.schemeEnd(at)
	}
	if mode == modeArray {
		parser.depth, parser.mark, parser.wrapped = 1, 0, false
	}
	parser.begin(at)
	parser.wordFrom = at
}

// byteAt returns the byte at pos, or 0 at the end of the text.
func (scan *kvScanner) byteAt(pos int) byte {
	if pos < len(scan.text) {
		return scan.text[pos]
	}
	return 0
}

// schemeEnd returns where a scheme word (bearer, basic, token) and the
// blanks after it end, when one starts at at; otherwise at.
func (scan *kvScanner) schemeEnd(at int) int {
	text := scan.text
	for _, scheme := range [...]string{"bearer", "basic", "token"} {
		length := foldedPrefix(text[at:], scheme)
		scan.work += len(scheme)
		if length < 0 {
			continue
		}
		end := at + length
		for end < len(text) && (text[end] == ' ' || text[end] == '\t') {
			end++
		}
		scan.work += end - at - length
		if end > at+length {
			return end
		}
	}
	return at
}

// lead reads what follows the separator, up to the first byte that says
// which form the value takes.
func (parser *valueParser) lead(scan *kvScanner, pos int, c byte, eot bool) {
	if eot {
		parser.done(pos, pos)
		return
	}
	switch c {
	case ' ', '\t':
		parser.lineIndent++
		return
	case '\r', '\f':
		return
	case '\n':
		parser.sawNewline, parser.lineIndent = true, 0
		return
	case '"':
		parser.mode = modeDoubleOpen
		parser.begin(pos)
		return
	case '\'':
		parser.mode = modeSingleOpen
		parser.begin(pos)
		return
	case '[':
		parser.content(scan, modeArray, pos+1)
		return
	case '\\':
		parser.mark, parser.mode = pos, modeBackslashes
		parser.begin(pos)
		return
	}
	if parser.sawNewline {
		parser.enterLines(pos)
		parser.lines(pos, scan.text, false)
		return
	}
	parser.keyLine(scan, pos, false)
}

// keyLine starts a value on the key's line at from, its first byte. With
// bare set, it is read as a header written as text or a bare value.
func (parser *valueParser) keyLine(scan *kvScanner, from int, bare bool) {
	text := scan.text
	parser.from, parser.firstSpace = from, -1
	parser.begin(from)
	parser.wordFrom = from
	if parser.kind == kindHeader {
		c := text[from]
		if !bare && (c == '|' || c == '>' || c == '#') {
			parser.indicatorOrComment(from, c)
			return
		}
		parser.valueStart, parser.mode = scan.schemeEnd(from), modeHeader
		return
	}
	if bare {
		parser.valueStart, parser.mode = from, modeBare
		return
	}
	for _, literal := range [...]string{"true", "false", "null"} {
		length := foldedPrefix(text[from:], literal)
		scan.work += len(literal)
		if length < 0 {
			continue
		}
		if end := from + length; end == len(text) || isSpace(text[end]) || strings.IndexByte(",]})", text[end]) >= 0 {
			if parser.swallowed {
				parser.valueStart, parser.mode = from, modeBare
				return
			}
			parser.kept, parser.valueDone = true, true
			return
		}
	}
	switch c := text[from]; c {
	case '|', '>', '#':
		parser.indicatorOrComment(from, c)
	case '{':
		parser.mode, parser.run = modeBrace, 0
		parser.valueStart = from
	default:
		parser.valueStart, parser.mode = from, modeBare
	}
}

func (parser *valueParser) indicatorOrComment(from int, c byte) {
	if c == '#' {
		parser.commentStart, parser.mode = from, modeComment
		return
	}
	parser.block, parser.mode, parser.run = true, modeIndicator, 0
	parser.commentStart = -1
}

// enterLines starts reading the lines after the key at the start of the
// line at at, or, from modeLead, at the first byte of a line whose
// indentation has been counted.
func (parser *valueParser) enterLines(at int) {
	if parser.mode != modeComment {
		parser.commentStart = -1
	}
	fromLead := parser.mode == modeLead || parser.mode == modeBackslashes
	parser.mode = modeLines
	parser.linesStart, parser.linesEnd = -1, -1
	parser.bodyStarted, parser.item, parser.dashed = false, false, false
	parser.mapping, parser.colon = false, false
	if fromLead {
		// The lines from the one after the key up to at are blank.
		parser.begin(at)
		if parser.wordFrom < 0 {
			parser.wordFrom = at
		}
		parser.mark = parser.lineIndent
		return
	}
	parser.lineIndent, parser.mark = 0, 0
	if parser.commentStart >= 0 {
		parser.begin(parser.commentStart)
		parser.wordFrom = parser.commentStart
	} else {
		parser.begin(at)
		if parser.wordFrom < 0 {
			parser.wordFrom = at
		}
	}
}

// lines reads the lines after a key, one byte at a time. After a block
// indicator the value is every following line indented more than the key's
// line. Without one it is the following lines that are items of a sequence
// (`- hunter2`, indented at least as much as the key's line) or plain text
// indented more than it. A line that reads as a mapping (`name: db`) ends
// the value: it is a field of its own, matched by name. Blank lines inside
// the value belong to it. parser.mark holds the indentation of the current
// line.
func (parser *valueParser) lines(pos int, text string, eot bool) {
	if parser.valueDone {
		return
	}
	c := byte('\n')
	if !eot {
		c = text[pos]
	}
	if c == '\n' {
		if parser.bodyStarted && !parser.endLine() {
			parser.finishLines()
			return
		}
		parser.bodyStarted, parser.item, parser.dashed, parser.mapping, parser.colon = false, false, false, false, false
		parser.mark = 0
		if eot {
			parser.finishLines()
		}
		return
	}
	if !parser.bodyStarted {
		switch c {
		case ' ', '\t':
			parser.mark++
			return
		case '\r':
			return
		}
		parser.bodyStarted, parser.bodyStart, parser.lastNonSpace = true, pos, pos+1
		parser.colon = c == ':'
		indent := parser.mark
		if parser.block {
			if indent <= parser.keyIndent {
				parser.finishLines()
			}
			return
		}
		if c == '-' {
			parser.dashed = true
			return
		}
		if indent <= parser.keyIndent {
			parser.finishLines()
		}
		return
	}
	if parser.dashed {
		parser.dashed = false
		if c == ' ' || c == '\t' {
			if parser.mark < parser.keyIndent {
				parser.finishLines()
				return
			}
			parser.item, parser.bodyStart, parser.lastNonSpace = true, -1, -1
			return
		}
		if parser.mark <= parser.keyIndent {
			parser.finishLines()
			return
		}
	}
	if c != ' ' && c != '\t' && c != '\r' {
		if parser.item && parser.bodyStart < 0 {
			parser.bodyStart = pos
		}
		parser.lastNonSpace = pos + 1
	}
	if parser.colon && (c == ' ' || c == '\t') {
		parser.mapping = true
	}
	if c != '\r' {
		parser.colon = c == ':'
	}
}

// endLine decides, at the end of a line whose body has started, whether the
// line belongs to the value, and records it when it does.
func (parser *valueParser) endLine() bool {
	if parser.dashed {
		// A line that is a lone dash: an empty item.
		if parser.block {
			parser.dashed = false
		} else {
			return false
		}
	}
	if !parser.block {
		if parser.bodyStart < 0 || parser.mapping || parser.colon {
			return false
		}
	}
	if parser.linesStart < 0 {
		parser.linesStart = parser.bodyStart
	}
	parser.linesEnd = parser.lastNonSpace
	return true
}

func (parser *valueParser) finishLines() {
	start, end := parser.linesStart, parser.linesEnd
	if parser.commentStart >= 0 {
		if start < 0 {
			end = parser.commentEnd
		} else {
			end = max(end, parser.commentEnd)
		}
		start = parser.commentStart
	}
	if start < 0 {
		start, end = parser.regionStart, parser.regionStart
	}
	parser.done(start, end)
}
