package ui

import (
	"fmt"
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
// catalog key that does not exist.
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
func checkTemplateKeys(trees map[string]*parse.Tree, base *Language) error {
	missing := map[string][]string{}
	for name, tree := range trees {
		if tree == nil || tree.Root == nil {
			continue
		}
		walkNode(tree.Root, func(key string) {
			if !base.Has(key) {
				missing[key] = append(missing[key], name)
			}
		})
	}
	if len(missing) == 0 {
		return nil
	}
	keys := make([]string, 0, len(missing))
	for key := range missing {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("ui: templates use message keys the base language pack (messages/" + BaseLanguageCode + ".toml) does not define:")
	for _, key := range keys {
		where := missing[key]
		sort.Strings(where)
		fmt.Fprintf(&b, "\n  %s (in %s)", key, strings.Join(dedupe(where), ", "))
	}
	return fmt.Errorf("%s", b.String())
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

// walkNode visits every node that can contain a pipeline. The type
// switch is exhaustive over the node kinds html/template produces from
// the syntax this package uses; anything unhandled simply contributes
// no keys, which is why templateKeys (used by the tests) exists to
// cross-check the count against a grep of the source.
func walkNode(n parse.Node, visit func(key string)) {
	switch node := n.(type) {
	case nil:
		return
	case *parse.ListNode:
		if node == nil {
			return
		}
		for _, child := range node.Nodes {
			walkNode(child, visit)
		}
	case *parse.ActionNode:
		walkPipe(node.Pipe, visit)
	case *parse.IfNode:
		walkBranch(&node.BranchNode, visit)
	case *parse.RangeNode:
		walkBranch(&node.BranchNode, visit)
	case *parse.WithNode:
		walkBranch(&node.BranchNode, visit)
	case *parse.TemplateNode:
		walkPipe(node.Pipe, visit)
	case *parse.PipeNode:
		walkPipe(node, visit)
	}
}

func walkBranch(b *parse.BranchNode, visit func(key string)) {
	walkPipe(b.Pipe, visit)
	walkNode(b.List, visit)
	walkNode(b.ElseList, visit)
}

func walkPipe(p *parse.PipeNode, visit func(key string)) {
	if p == nil {
		return
	}
	for _, cmd := range p.Cmds {
		if len(cmd.Args) == 0 {
			continue
		}
		if ident, ok := cmd.Args[0].(*parse.IdentifierNode); ok && catalogFuncs[ident.Ident] {
			if len(cmd.Args) > 1 {
				if str, ok := cmd.Args[1].(*parse.StringNode); ok {
					visit(str.Text)
				}
			}
		}
		// Arguments can themselves be parenthesised pipelines.
		for _, arg := range cmd.Args {
			if sub, ok := arg.(*parse.PipeNode); ok {
				walkPipe(sub, visit)
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
		walkNode(tree.Root, func(key string) { found[key] = true })
	}
	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
