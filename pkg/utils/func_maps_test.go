package utils

import (
	"html/template"
	"os"
	"strings"
	"testing"
)

func TestTemplateDict_Set(t *testing.T) {
	d := TemplateDict{}
	d.Set("a", 1).Set("b", "x")
	if d["a"] != 1 || d["b"] != "x" {
		t.Fatalf("unexpected: %v", d)
	}
}

func TestRegisterAndGetIncludeFunc(t *testing.T) {
	tmpl := template.Must(template.New("root").Parse(`{{define "child"}}hello {{.Name}}{{end}}`))
	RegisterTemplate("root-tmpl", tmpl)

	inc := GetIncludeFunc("root-tmpl")
	got, err := inc("child", map[string]string{"Name": "world"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("got %q", got)
	}

	// Unknown registry entry → error.
	if _, err := GetIncludeFunc("missing")("child", nil); err == nil {
		t.Error("expected error for unregistered template")
	}

	// Existing registry but unknown sub-template name → error.
	if _, err := inc("nope", nil); err == nil {
		t.Error("expected error for missing sub-template")
	}
}

func TestGetTemplateFuncMaps_StringHelpers(t *testing.T) {
	fm := GetTemplateFuncMaps()
	if got := fm["upper"].(func(string) string)("aB"); got != "AB" {
		t.Errorf("upper = %q", got)
	}
	if got := fm["lower"].(func(string) string)("aB"); got != "ab" {
		t.Errorf("lower = %q", got)
	}
	if got := fm["title"].(func(string) string)("hello world"); !strings.HasPrefix(got, "Hello") {
		t.Errorf("title = %q", got)
	}
	if got := fm["toString"].(func(interface{}) string)(42); got != "42" {
		t.Errorf("toString = %q", got)
	}
	if got := fm["truncate"].(func(string, int) string)("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := fm["truncate"].(func(string, int) string)("ab", 10); got != "ab" {
		t.Errorf("truncate no-op = %q", got)
	}
	if got := fm["slice"].(func(interface{}, int, int) string)("hello", 1, 4); got != "ell" {
		t.Errorf("slice = %q", got)
	}
	if got := fm["slice"].(func(interface{}, int, int) string)("hello", -1, 2); got != "hello" {
		t.Errorf("slice bad-range fallback = %q", got)
	}
	if got := fm["slice"].(func(interface{}, int, int) string)(123, 0, 1); got != "" {
		t.Errorf("slice non-string = %q", got)
	}
}

func TestGetTemplateFuncMaps_Default(t *testing.T) {
	fm := GetTemplateFuncMaps()
	def := fm["default"].(func(interface{}, interface{}) interface{})
	if got := def("", "fallback"); got != "fallback" {
		t.Errorf("default empty = %v", got)
	}
	if got := def("x", "fallback"); got != "x" {
		t.Errorf("default non-empty = %v", got)
	}
	if got := def(nil, "fallback"); got != "fallback" {
		t.Errorf("default nil = %v", got)
	}
	if got := def(0, "fallback"); got != 0 {
		t.Errorf("default zero-int passes through = %v", got)
	}
}

func TestGetTemplateFuncMaps_Len(t *testing.T) {
	fm := GetTemplateFuncMaps()
	l := fm["len"].(func(interface{}) int)
	if l("abc") != 3 {
		t.Error("len string")
	}
	if l([]interface{}{1, 2}) != 2 {
		t.Error("len []interface{}")
	}
	if l([]string{"a", "b", "c"}) != 3 {
		t.Error("len []string")
	}
	if l(map[string]interface{}{"a": 1}) != 1 {
		t.Error("len map")
	}
	if l(TemplateDict{"a": 1, "b": 2}) != 2 {
		t.Error("len TemplateDict")
	}
	if l(123) != 0 {
		t.Error("len fallback")
	}
}

func TestGetTemplateFuncMaps_FormatTime(t *testing.T) {
	fm := GetTemplateFuncMaps()
	ft := fm["formatTime"].(func(string) string)
	if got := ft("2024-01-02T03:04:05Z"); got != "2024-01-02 03:04:05" {
		t.Errorf("formatTime = %q", got)
	}
	if got := ft("garbage"); got != "Invalid time" {
		t.Errorf("formatTime invalid = %q", got)
	}
}

func TestGetTemplateFuncMaps_Env(t *testing.T) {
	t.Setenv("TEST_ENV_VAR_FUNCMAP", "hi")
	fm := GetTemplateFuncMaps()
	if got := fm["env"].(func(string) string)("TEST_ENV_VAR_FUNCMAP"); got != "hi" {
		t.Errorf("env = %q", got)
	}
	_ = os.Unsetenv("TEST_ENV_VAR_FUNCMAP")
}

func TestGetTemplateFuncMaps_Dict(t *testing.T) {
	fm := GetTemplateFuncMaps()
	dict := fm["dict"].(func(...interface{}) (TemplateDict, error))
	d, err := dict("a", 1, "b", "two")
	if err != nil {
		t.Fatal(err)
	}
	if d["a"] != 1 || d["b"] != "two" {
		t.Errorf("dict = %v", d)
	}
	if _, err := dict("only-one-arg"); err == nil {
		t.Error("expected odd-arg error")
	}
	if _, err := dict(1, "value"); err == nil {
		t.Error("expected non-string key error")
	}
}

func TestGetTemplateFuncMaps_ListAppendStringSlice(t *testing.T) {
	fm := GetTemplateFuncMaps()
	list := fm["list"].(func(...interface{}) []interface{})("a", "b")
	if len(list) != 2 {
		t.Fatalf("list len %d", len(list))
	}
	app := fm["append"].(func([]interface{}, ...interface{}) []interface{})(list, "c")
	if len(app) != 3 || app[2] != "c" {
		t.Errorf("append = %v", app)
	}
	ss := fm["stringSlice"].(func([]interface{}) []string)([]interface{}{1, "x", true})
	if len(ss) != 3 || ss[0] != "1" || ss[1] != "x" || ss[2] != "true" {
		t.Errorf("stringSlice = %v", ss)
	}
}

func TestGetTemplateFuncMaps_Index(t *testing.T) {
	fm := GetTemplateFuncMaps()
	idx := fm["index"].(func(interface{}, interface{}) interface{})

	if got := idx(map[string]interface{}{"a": 1}, "a"); got != 1 {
		t.Errorf("map string key = %v", got)
	}
	// Non-string key gets stringified for string-keyed maps.
	if got := idx(map[string]interface{}{"7": "ok"}, 7); got != "ok" {
		t.Errorf("map coerced key = %v", got)
	}
	if got := idx([]interface{}{"a", "b", "c"}, 1); got != "b" {
		t.Errorf("slice int = %v", got)
	}
	if got := idx([]interface{}{"a", "b"}, int64(1)); got != "b" {
		t.Errorf("slice int64 = %v", got)
	}
	if got := idx([]interface{}{"a", "b"}, float64(0)); got != "a" {
		t.Errorf("slice float64 = %v", got)
	}
	if got := idx([]interface{}{"a"}, 5); got != nil {
		t.Errorf("slice OOB = %v", got)
	}
	if got := idx(nil, "x"); got != nil {
		t.Errorf("nil obj = %v", got)
	}
	type S struct{ Name string }
	if got := idx(S{Name: "n"}, "Name"); got != "n" {
		t.Errorf("struct field = %v", got)
	}
	if got := idx(S{}, "Missing"); got != nil {
		t.Errorf("struct missing field = %v", got)
	}
}

func TestGetTemplateFuncMaps_Comparisons(t *testing.T) {
	fm := GetTemplateFuncMaps()
	eq := fm["eq"].(func(interface{}, interface{}) bool)
	ne := fm["ne"].(func(interface{}, interface{}) bool)
	lt := fm["lt"].(func(interface{}, interface{}) bool)
	gt := fm["gt"].(func(interface{}, interface{}) bool)

	if !eq(1, 1) || eq(1, 2) {
		t.Error("eq")
	}
	if !ne(1, 2) || ne(1, 1) {
		t.Error("ne")
	}
	if !lt(1, 2) || lt(2, 1) {
		t.Error("lt int")
	}
	if !lt("a", "b") || lt("b", "a") {
		t.Error("lt string")
	}
	if !gt(2, 1) || gt(1, 2) {
		t.Error("gt int")
	}
	if !gt("b", "a") {
		t.Error("gt string")
	}
	// Mixed types → false.
	if lt(1, "x") || gt(1, "x") {
		t.Error("lt/gt mixed types should be false")
	}
}

func TestGetTemplateFuncMaps_Logical(t *testing.T) {
	fm := GetTemplateFuncMaps()
	and := fm["and"].(func(...interface{}) bool)
	or := fm["or"].(func(...interface{}) interface{})
	not := fm["not"].(func(interface{}) bool)

	if !and(true, 1, "x") {
		t.Error("and all-truthy")
	}
	if and(true, 0) {
		t.Error("and contains falsy")
	}
	if got := or(false, "", "first-truthy"); got != "first-truthy" {
		t.Errorf("or = %v", got)
	}
	if got := or(false, ""); got != "" {
		// returns the last arg if none truthy
		t.Errorf("or no-truthy = %v", got)
	}
	if !not(false) || not(true) {
		t.Error("not")
	}
}

func TestGetTemplateFuncMaps_AddMixedNumeric(t *testing.T) {
	fm := GetTemplateFuncMaps()
	add := fm["add"].(func(interface{}, interface{}) interface{})
	if add(1, 2) != 3 {
		t.Error("int+int")
	}
	if add(int64(1), int64(2)) != int64(3) {
		t.Error("int64+int64")
	}
	if add(1.5, 2.5) != 4.0 {
		t.Error("float+float")
	}
	if add(1, int64(2)) != int64(3) {
		t.Error("int+int64")
	}
	if add(1, 2.5) != 3.5 {
		t.Error("int+float")
	}
	if add(int64(1), 2.5) != 3.5 {
		t.Error("int64+float")
	}
	if got := add("a", "b"); got != "ab" {
		t.Errorf("string concat fallback = %v", got)
	}
}

func TestGetTemplateFuncMaps_RegexMatch(t *testing.T) {
	fm := GetTemplateFuncMaps()
	rm := fm["regexMatch"].(func(string, string) bool)
	if !rm(`^foo`, "foobar") {
		t.Error("expected match")
	}
	if rm(`^foo`, "barfoo") {
		t.Error("expected no match")
	}
}

func TestGetTemplateFuncMaps_FormatTimeLike(t *testing.T) {
	fm := GetTemplateFuncMaps()
	format := fm["format"].(func(string, interface{}) string)
	got := format("2006-01-02", "2024-05-06T07:08:09Z")
	if got != "2024-05-06" {
		t.Errorf("format string-time = %q", got)
	}
	got = format("2006-01-02", "garbage")
	if got != "Invalid time format" {
		t.Errorf("format invalid = %q", got)
	}
	if !strings.Contains(format("2006", 123), "Unsupported") {
		t.Error("format unsupported type")
	}
}

func TestGetTemplateFuncMaps_IncludeStub(t *testing.T) {
	fm := GetTemplateFuncMaps()
	inc := fm["include"].(func(string, interface{}) (string, error))
	s, err := inc("x", nil)
	if err != nil || s != "" {
		t.Errorf("stub include = (%q, %v)", s, err)
	}
}

func TestGetTemplateFuncMaps_RenderEndToEnd(t *testing.T) {
	// `default <val> <def>` returns `def` only when `val` is empty.
	tmpl, err := template.New("t").Funcs(GetTemplateFuncMaps()).Parse(
		`{{ upper "hello" }} {{ default "" "fb" }} {{ default "x" "fb" }}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "HELLO fb x" {
		t.Errorf("render = %q", got)
	}
}

func TestIsTrue_ViaTemplate(t *testing.T) {
	// Indirectly exercise isTrue via the `and` / `not` template funcs.
	fm := GetTemplateFuncMaps()
	and := fm["and"].(func(...interface{}) bool)
	not := fm["not"].(func(interface{}) bool)

	type ptr struct{}
	var nilPtr *ptr
	if !and(true, 1, "x", 1.5, []int{1}, map[string]int{"a": 1}, struct{}{}) {
		t.Error("expected all truthy values to pass")
	}
	for _, v := range []interface{}{false, 0, "", 0.0, []int{}, map[string]int{}, nilPtr, nil} {
		if and(v) {
			t.Errorf("expected falsy: %#v", v)
		}
		if !not(v) {
			t.Errorf("not(%#v) should be true", v)
		}
	}
}
