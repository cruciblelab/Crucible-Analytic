package ui

import (
	"html/template"
	"strings"
	"testing"
	"text/template/parse"
)

// TestACatalogCallWithTheWrongNumberOfValuesStopsTheBinary.
//
// html/template counts a function's arguments when the action runs, not
// when the template is parsed. So "{{t "key" .Value}}" parses, the panel
// starts, and the page that draws it answers 500 - on the first request
// that reaches the branch. C7.2's operator branch on the recovery codes
// page was one for thirty-three days: nothing drew it until B3h wrote the
// handler, and the handler's first test got the 500.
//
// Each case is one template; the check has to name the bad ones and pass
// the good ones - including a pipe, where the value a command receives
// comes from the command before it rather than from its own arguments.
func TestACatalogCallWithTheWrongNumberOfValuesStopsTheBinary(t *testing.T) {
	base := testCatalogs(t).Base()
	const key = "gezinme.atla"
	cases := []struct {
		name, src string
		wrong     bool
	}{
		{"t with its key", `{{t "` + key + `"}}`, false},
		{"t with a value too", `{{t "` + key + `" .}}`, true},
		{"t with no key", `{{t}}`, true},
		{"t handed its key by a pipe", `{{"` + key + `" | t}}`, false},
		{"t with a key and a piped value", `{{. | t "` + key + `"}}`, true},
		{"tf with a value", `{{tf "` + key + `" .}}`, false},
		{"tf with two values", `{{tf "` + key + `" . .}}`, false},
		{"tf with only its key", `{{tf "` + key + `"}}`, false},
		{"tf with nothing", `{{tf}}`, true},
		{"t inside a branch", `{{if .}}{{t "` + key + `" .}}{{end}}`, true},
		{"t inside a parenthesised argument", `{{printf "%s" (t "` + key + `" .)}}`, true},
		// Not a catalog function: the count comes from the signature, so
		// every function the pages are drawn with is held to it.
		{"asset with its name", `{{asset "panel.css"}}`, false},
		{"asset with two", `{{asset "panel.css" .}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			funcs := fixtureFuncs(base)
			tmpl, err := template.New("sinama").Funcs(funcs).Parse(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			err = checkTemplateKeys(map[string]*parse.Tree{"sinama": tmpl.Tree}, base, funcs)
			if tc.wrong && (err == nil || !strings.Contains(err.Error(), "sinama: ")) {
				t.Errorf("%s passed the check; it answers 500 when drawn (err %v)", tc.src, err)
			}
			if !tc.wrong && err != nil {
				t.Errorf("%s was refused: %v", tc.src, err)
			}
		})
	}
}
