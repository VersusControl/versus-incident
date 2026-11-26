package main

import (
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// Credential-shaped inputs are built from parts here too, so this file
// passes its own scan.
var (
	temporaryKeyID = "ASIA" + "ABCDEFGHIJKLMNOP"
	secretKey      = "Ab1/" + strings.Repeat("Cd2+", 9)
	gitHubToken    = "ghp_" + strings.Repeat("a1B2", 9)
	slackToken     = "xoxb-" + "2410-1234567890-AbCdEfGhIjKl"
	openAIKey      = "sk-" + "proj-AbCdEf0123456789AbCdEfGhIjKlMnOpQr"
	jwt            = "eyJhbGciOiJIUzI1NiJ9" + ".eyJzdWIiOiIxIn0.abc"
	pemHeader      = "-----BEGIN RSA " + "PRIVATE KEY-----"
)

func scanText(t *testing.T, text string) []finding {
	t.Helper()
	found, err := scanLines("input", strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The line secret scanning raised an alert for, a made-up temporary AWS key
// ID in a test table, is a finding; so is every other credential shape.
func TestScanFindsEveryCredentialShape(t *testing.T) {
	for _, tc := range []struct{ rule, line, match string }{
		{"AWS access key ID", `	{"temporary key", "` + temporaryKeyID + `"},`, temporaryKeyID},
		{"AWS secret access key", "aws_secret_access_key = " + secretKey, secretKey},
		{"GitHub token", "TOKEN=" + gitHubToken, gitHubToken},
		{"Slack token", "posting via " + slackToken + " now", slackToken},
		{"OpenAI key", "calling model with " + openAIKey, openAIKey},
		{"JWT", "token=" + jwt, jwt},
		{"private key block", pemHeader, pemHeader},
		{"AWS access key ID", `	creds := "id\n` + temporaryKeyID + `"`, temporaryKeyID},
		{"AWS access key ID", `	creds := "id\t` + temporaryKeyID + `"`, temporaryKeyID},
		{"AWS access key ID", `	creds := "{\"id\":\"` + temporaryKeyID + `\"}"`, temporaryKeyID},
		{"Slack token", `	body := "token\n` + slackToken + `"`, slackToken},
		{"GitHub token", `	env := "A=1\nTOKEN=` + gitHubToken + `"`, gitHubToken},
	} {
		t.Run(tc.rule+" in "+tc.line, func(t *testing.T) {
			found := scanText(t, "first line\n"+tc.line+"\n")
			if len(found) != 1 || found[0].Rule != tc.rule || found[0].Match != tc.match || found[0].Line != 2 {
				t.Fatalf("scan(%q) = %+v, want one %s %q on line 2", tc.line, found, tc.rule, tc.match)
			}
		})
	}
}

// The AWS documentation examples, values built at runtime, placeholders and
// look-alikes that are not credentials pass.
func TestScanAllowsExamplesPlaceholdersAndLookAlikes(t *testing.T) {
	for name, line := range map[string]string{
		"AWS example key ID":         `key := "AKIAIOSFODNN7EXAMPLE"`,
		"AWS example secret":         `secret = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
		"key ID built at runtime":    `id := "ASIA" + "ABCDEFGHIJKLMNOP"`,
		"key ID inside a longer run": `XAKIAABCDEFGHIJKLMNOP and AKIAABCDEFGHIJKLMNOPQ`,
		"placeholder":                `aws_access_key_id = <access-key-id>`,
		"short Slack placeholder":    `token: "xoxb-your-slack-token"`,
		"short OpenAI placeholder":   `api_key: "sk-test-key"`,
		"short GitHub placeholder":   `GITHUB_TOKEN=ghp_xxxxxxxxxxxx`,
		"word ending in the prefix":  `disk-0123456789012345678901234 task-abcdefghijklmnopqrstuvwxyz`,
		"hex digest":                 `sha1 0123456789abcdef0123456789abcdef01234567`,
		"long base64 run":            `sha512-` + strings.Repeat("Ab1+", 22),
		"JWT header alone":           `header eyJhbGciOiJIUzI1NiJ9.T0K3N`,
		"public key block":           `-----BEGIN PUBLIC KEY-----`,
	} {
		if found := scanText(t, line); len(found) != 0 {
			t.Errorf("%s: scan(%q) = %+v, want none", name, line, found)
		}
	}
}

// The tree walk scans code, config, scripts, templates, chart test fixtures,
// docs, Dockerfiles and Makefiles, and skips installed packages, build
// output, local test data and other file kinds.
func TestScanTreeScansSourceKindsOnly(t *testing.T) {
	leak := []byte(`id := "` + temporaryKeyID + `"` + "\n")
	tree := fstest.MapFS{
		"pkg/a/a_test.go":              {Data: leak},
		"helm/values.yaml":             {Data: leak},
		"src/page.md":                  {Data: leak},
		"ui/src/a.test.tsx":            {Data: leak},
		"ui/node_modules/x/index.ts":   {Data: leak},
		"ui/dist/app.json":             {Data: leak},
		"testdata/capture.json":        {Data: leak},
		"pkg/a/clean.go":               {Data: []byte("package a\n")},
		"config/alert.tmpl":            {Data: leak},
		"helm/templates/_x.tpl":        {Data: leak},
		"helm/tests/01.args":           {Data: leak},
		"helm/tests/01.assert":         {Data: leak},
		"scripts/run.sh":               {Data: leak},
		"tools/gen.py":                 {Data: leak},
		"ui/vite.config.js":            {Data: leak},
		"ui/build.mjs":                 {Data: leak},
		"pkg/storage/migrations/1.sql": {Data: leak},
		"deploy/.env.example":          {Data: leak},
		"pyproject.toml":               {Data: leak},
		"setup.ini":                    {Data: leak},
		"Dockerfile":                   {Data: leak},
		"Dockerfile.dev":               {Data: leak},
		"Makefile":                     {Data: leak},
		"notes.txt":                    {Data: leak},
		"image.png":                    {Data: leak},
	}
	found, err := scanTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range found {
		paths = append(paths, f.Path)
	}
	want := strings.Join([]string{
		"Dockerfile", "Dockerfile.dev", "Makefile", "config/alert.tmpl", "deploy/.env.example",
		"helm/templates/_x.tpl", "helm/tests/01.args", "helm/tests/01.assert", "helm/values.yaml",
		"pkg/a/a_test.go", "pkg/storage/migrations/1.sql", "pyproject.toml", "scripts/run.sh", "setup.ini",
		"src/page.md", "tools/gen.py", "ui/build.mjs", "ui/src/a.test.tsx", "ui/vite.config.js",
	}, ",")
	sort.Strings(paths)
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("findings in %s, want %s", got, want)
	}
}
