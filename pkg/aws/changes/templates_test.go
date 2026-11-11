package changes

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write the rendered templates and a regenerated expectation table to testdata/aws for local review")

// templateSnapshotDir receives the -update output. It is the gitignored local
// test-data folder: no test reads it.
var templateSnapshotDir = filepath.Join("..", "..", "..", "testdata", "aws")

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

func sampleFields(template Template) map[string]string {
	fields := make(map[string]string, len(template.Fields))
	for _, field := range template.Fields {
		fields[field.Name] = field.Name + "-1"
	}
	return fields
}

func TestTemplateTableIsComplete(t *testing.T) {
	table := Templates()
	if len(table) != 40 {
		t.Fatalf("templates = %d, want 40", len(table))
	}
	seen := map[string]bool{}
	for _, template := range table {
		if seen[template.ID] {
			t.Fatalf("duplicate template %s", template.ID)
		}
		seen[template.ID] = true
		if found, ok := Lookup(template.EventSource, template.EventName); !ok || found.ID != template.ID {
			t.Fatalf("lookup %s", template.ID)
		}
		declared := map[string]bool{}
		for _, field := range template.Fields {
			declared[field.Name] = true
			if !strings.HasPrefix(field.Path, "requestParameters.") && !strings.HasPrefix(field.Path, "responseElements.") {
				t.Errorf("%s field %s reads outside the allowlisted documents", template.ID, field.Name)
			}
		}
		for _, name := range template.ResourceFields {
			if !declared[name] {
				t.Errorf("%s resource field %s is not declared", template.ID, name)
			}
		}
		for _, text := range template.Texts {
			for _, match := range placeholder.FindAllStringSubmatch(text, -1) {
				if !declared[match[1]] {
					t.Errorf("%s text uses undeclared %s", template.ID, match[1])
				}
			}
		}
		if last := template.Texts[len(template.Texts)-1]; placeholder.MatchString(last) {
			t.Errorf("%s has no plain fallback text", template.ID)
		}
		if _, ok := serviceNames[template.EventSource]; !ok {
			t.Errorf("%s event source %s has no catalog service", template.ID, template.EventSource)
		}
	}
}

// renderedTemplate is one template rendered with all of its fields and with
// none.
type renderedTemplate struct {
	id, event, full, minimal string
}

func renderTemplates() []renderedTemplate {
	var out []renderedTemplate
	for _, template := range Templates() {
		out = append(out, renderedTemplate{id: template.ID, event: template.EventSource + " " + template.EventName,
			full: template.Render(sampleFields(template)), minimal: template.Render(nil)})
	}
	return out
}

// genericSummaries are events no template covers, with the summary each
// gets.
var genericSummaries = []struct{ source, event, want string }{
	{"s3.amazonaws.com", "PutBucketTagging", "Put bucket tagging (S3)"},
	{"lambda.amazonaws.com", "UpdateFunctionUrlConfig20211031", "Update function url config (Lambda)"},
	{"unknown.amazonaws.com", "CreateThing", "Create thing (unknown.amazonaws.com)"},
}

// TestTemplateWording renders every template with all of its fields and
// with none and compares the text with wantTemplates, so a wording change is
// a reviewed edit of that table. With -update the rendering and a
// regenerated table are written to testdata/aws for review; the test still
// compares.
func TestTemplateWording(t *testing.T) {
	got := renderTemplates()
	if *update {
		writeTemplateSnapshot(t, got)
	}
	if len(got) != len(wantTemplates) {
		t.Fatalf("%d templates, want %d", len(got), len(wantTemplates))
	}
	for i, want := range wantTemplates {
		if got[i] != want {
			t.Errorf("template %d:\n got: %+v\nwant: %+v", i, got[i], want)
		}
	}
	for _, generic := range genericSummaries {
		if got := GenericSummary(generic.source, generic.event); got != generic.want {
			t.Errorf("GenericSummary(%s, %s) = %q, want %q", generic.source, generic.event, got, generic.want)
		}
	}
}

// writeTemplateSnapshot writes the rendering and a regenerated
// wantTemplates table under the gitignored testdata folder.
func writeTemplateSnapshot(t *testing.T, rendered []renderedTemplate) {
	t.Helper()
	if err := os.MkdirAll(templateSnapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var text, table strings.Builder
	table.WriteString("var wantTemplates = []renderedTemplate{\n")
	for _, r := range rendered {
		text.WriteString(r.id + " | " + r.event + "\n  full:    " + r.full + "\n  minimal: " + r.minimal + "\n")
		table.WriteString("\t{" + strconv.Quote(r.id) + ", " + strconv.Quote(r.event) + ",\n\t\t" + strconv.Quote(r.full) +
			",\n\t\t" + strconv.Quote(r.minimal) + "},\n")
	}
	table.WriteString("}\n")
	for _, generic := range genericSummaries {
		text.WriteString("generic | " + GenericSummary(generic.source, generic.event) + "\n")
	}
	for name, body := range map[string]string{"change_templates.txt": text.String(), "change_templates.go.txt": table.String()} {
		if err := os.WriteFile(filepath.Join(templateSnapshotDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRenderFallsBackToFewerFields(t *testing.T) {
	template, _ := Lookup("ecs.amazonaws.com", "UpdateService")
	if got := template.Render(map[string]string{"service": "web", "desired": "3"}); got != "Service web scaled to 3 tasks" {
		t.Fatalf("render = %q", got)
	}
	if got := template.Render(map[string]string{"service": "web"}); got != "Service web updated" {
		t.Fatalf("render = %q", got)
	}
	if ServiceForSource("monitoring.amazonaws.com") != "cloudwatch" || ServiceForSource("foo.amazonaws.com") != "foo" {
		t.Fatal("service mapping")
	}
}
