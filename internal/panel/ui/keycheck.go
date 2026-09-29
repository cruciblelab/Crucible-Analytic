package ui

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"text/template/parse"
)

// catalogFuncs are the template functions whose first argument is a
// catalog key. Adding a new one means adding it here, or its keys stop
// being checked - which is why the list is tiny and stays tiny.
var catalogFuncs = map[string]bool{
	"t":  true,
	"tf": true,
}

// checkTemplateKeys walks the parsed templates and reports every
// catalog key that does not exist, and every call of one of funcs with a
// number of arguments the function does not take.
//
// This runs at startup and its failure stops the binary. The reason is
// the failure mode it replaces: a template naming a key nobody wrote
// renders as a marker in the middle of a sentence, on a page somebody
// may not open for weeks. Turning that into "the panel will not start"
// moves the discovery from a customer to whoever changed the template,
// which is the only person who can fix it cheaply.
//
// Only constant keys can be checked. A key assembled at runtime -
// "hata." plus a status code, say - is invisible to this walk, which is
// why Catalog.T still has a visible fallback and why the tests below
// check the computed families explicitly.
//
// The argument counts are checked here because nothing else checks them
// before a person does: html/template asks a function how many arguments
// it takes only when the action runs. A t with a value behind it parses,
// the binary starts, and the one page that draws it answers 500 - which,
// for a branch no handler reaches yet, is the day somebody writes the
// handler. C7.2's operator branch on the codes page was such a line for
// thirty-three days, found by B3h. The counts come from the functions'
// own signatures, so a function added to the map is checked without
// anybody adding it anywhere else.
func checkTemplateKeys(trees map[string]*parse.Tree, base *Language, funcs map[string]any) error {
	counts := map[string]argCount{}
	for name, fn := range funcs {
		if c, ok := argCountOf(fn); ok {
			counts[name] = c
		}
	}
	missing := map[string][]string{}
	var miscounted []string
	for name, tree := range trees {
		if tree == nil || tree.Root == nil {
			continue
		}
		walkNode(tree.Root, counts, func(key string) {
			if !base.Has(key) {
				missing[key] = append(missing[key], name)
			}
		}, func(call string, want argCount, got int) {
			miscounted = append(miscounted, fmt.Sprintf("%s: %s takes %s, given %d", name, call, want, got))
		})
	}
	if len(missing) == 0 && len(miscounted) == 0 {
		return nil
	}
	var b strings.Builder
	if len(missing) > 0 {
		keys := make([]string, 0, len(missing))
		for key := range missing {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteString("ui: templates use message keys the base language pack (messages/" + BaseLanguageCode + ".toml) does not define:")
		for _, key := range keys {
			where := missing[key]
			sort.Strings(where)
			fmt.Fprintf(&b, "\n  %s (in %s)", key, strings.Join(dedupe(where), ", "))
		}
	}
	if len(miscounted) > 0 {
		sort.Strings(miscounted)
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("ui: templates call functions with a number of arguments they do not take " +
			"(html/template counts them only when the page is drawn; a sentence with a value is tf's, not t's):")
		for _, m := range miscounted {
			b.WriteString("\n  " + m)
		}
	}
	return fmt.Errorf("%s", b.String())
}

// argCount is how many arguments a template function takes: exactly min,
// or min and more when variadic.
type argCount struct {
	min      int
	variadic bool
}

func (c argCount) String() string {
	if c.variadic {
		return fmt.Sprintf("%d or more arguments", c.min)
	}
	return fmt.Sprintf("%d arguments", c.min)
}

func (c argCount) accepts(n int) bool {
	return n == c.min || (c.variadic && n > c.min)
}

// argCountOf reads a function's count from its signature.
func argCountOf(fn any) (argCount, bool) {
	t := reflect.TypeOf(fn)
	if t == nil || t.Kind() != reflect.Func {
		return argCount{}, false
	}
	if t.IsVariadic() {
		return argCount{min: t.NumIn() - 1, variadic: true}, true
	}
	return argCount{min: t.NumIn()}, true
}

func dedupe(in []string) []string {
	out := in[:0:0]
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// miscount reports a call given the wrong number of arguments: the
// function, what it takes, and what it was given.
type miscount func(call string, want argCount, got int)

// walkNode visits every node that can contain a pipeline. The type
// switch is exhaustive over the node kinds html/template produces from
// the syntax this package uses; anything unhandled simply contributes
// no keys, which is why templateKeys (used by the tests) exists to
// cross-check the count against a grep of the source.
func walkNode(n parse.Node, counts map[string]argCount, visit func(key string), wrong miscount) {
	switch node := n.(type) {
	case nil:
		return
	case *parse.ListNode:
		if node == nil {
			return
		}
		for _, child := range node.Nodes {
			walkNode(child, counts, visit, wrong)
		}
	case *parse.ActionNode:
		walkPipe(node.Pipe, counts, visit, wrong)
	case *parse.IfNode:
		walkBranch(&node.BranchNode, counts, visit, wrong)
	case *parse.RangeNode:
		walkBranch(&node.BranchNode, counts, visit, wrong)
	case *parse.WithNode:
		walkBranch(&node.BranchNode, counts, visit, wrong)
	case *parse.TemplateNode:
		walkPipe(node.Pipe, counts, visit, wrong)
	case *parse.PipeNode:
		walkPipe(node, counts, visit, wrong)
	}
}

func walkBranch(b *parse.BranchNode, counts map[string]argCount, visit func(key string), wrong miscount) {
	walkPipe(b.Pipe, counts, visit, wrong)
	walkNode(b.List, counts, visit, wrong)
	walkNode(b.ElseList, counts, visit, wrong)
}

func walkPipe(p *parse.PipeNode, counts map[string]argCount, visit func(key string), wrong miscount) {
	if p == nil {
		return
	}
	for i, cmd := range p.Cmds {
		if len(cmd.Args) == 0 {
			continue
		}
		if ident, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
			if catalogFuncs[ident.Ident] && len(cmd.Args) > 1 {
				if str, ok := cmd.Args[1].(*parse.StringNode); ok {
					visit(str.Text)
				}
			}
			if want, known := counts[ident.Ident]; known {
				// Everything after the function's name, plus the value a
				// pipe hands every command but the first.
				given := len(cmd.Args) - 1
				if i > 0 {
					given++
				}
				if !want.accepts(given) {
					wrong(ident.Ident, want, given)
				}
			}
		}
		// Arguments can themselves be parenthesised pipelines.
		for _, arg := range cmd.Args {
			if sub, ok := arg.(*parse.PipeNode); ok {
				walkPipe(sub, counts, visit, wrong)
			}
		}
	}
}

// templateKeys returns every constant catalog key the trees reference,
// sorted. Used by the test that checks the other direction: a catalog
// entry no template and no handler names is dead text, and dead text is
// how a catalog grows into something nobody trusts.
func templateKeys(trees map[string]*parse.Tree) []string {
	found := map[string]bool{}
	for _, tree := range trees {
		if tree == nil || tree.Root == nil {
			continue
		}
		walkNode(tree.Root, nil, func(key string) { found[key] = true }, func(string, argCount, int) {})
	}
	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
