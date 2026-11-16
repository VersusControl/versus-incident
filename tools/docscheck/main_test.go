package main

import (
	"reflect"
	"testing"
	"testing/fstest"
)

func TestCheck(t *testing.T) {
	page := "# Guide\n" +
		"[relative](./other.md) [no ext](other) [folder](/agent/) [rooted](/agent/setup)\n" +
		"![image](../docs/images/a.png) [anchor](#top) [query](/agent/setup?id=step-2)\n" +
		"[external](https://example.com/x.md) [mail](mailto:a@b.c)\n" +
		"<img src=\"docs/images/a.png\"> <a href=\"/agent/setup\">x</a>\n" +
		"`[inline code](missing.md)`\n" +
		"```\n[fenced](missing.md)\n```\n" +
		"[gone](./missing.md)\n" +
		"![gone image](../docs/images/missing.png)\n" +
		"[outside the site](../../ROADMAP.md)\n" +
		"<img src=\"docs/images/missing.png\">\n"
	site := fstest.MapFS{
		"guide/page.md":       {Data: []byte(page)},
		"guide/other.md":      {Data: []byte("x")},
		"agent/README.md":     {Data: []byte("x")},
		"agent/setup.md":      {Data: []byte("x")},
		"docs/images/a.png":   {Data: []byte("x")},
		"docs/images/b.png":   {Data: []byte("x")},
		"guide/not-markdown":  {Data: []byte("[skip](missing.md)")},
		"introduction.md":     {Data: []byte("[guide](guide/page.md)")},
		"agent/sub/nested.md": {Data: []byte("[up](../setup.md) [root](/introduction)")},
	}
	got, err := Check(site)
	if err != nil {
		t.Fatal(err)
	}
	want := []Broken{
		{File: "guide/page.md", Line: 10, Target: "./missing.md"},
		{File: "guide/page.md", Line: 11, Target: "../docs/images/missing.png"},
		{File: "guide/page.md", Line: 12, Target: "../../ROADMAP.md"},
		{File: "guide/page.md", Line: 13, Target: "docs/images/missing.png"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Check() =\n%v\nwant\n%v", got, want)
	}
}
