package ui

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A spanning cell spans the whole row.
//
// # The drift this catches, which has happened twice in one file
//
// saglik.html's services table was written with five columns and a
// detail row under each service carrying colspan="5". Then A2 added a
// profile column and 5b added an IP-token column, and the spanning rows
// stayed at five - so the last two columns of the error row were empty,
// and the "this is the panel itself" row stopped reaching the end of the
// table. Nothing failed, nothing looked obviously wrong in the markup,
// and the only way to notice was to look at a rendered page with a
// service that had an error.
//
// That is the shape of defect this project keeps writing tests for
// rather than comments: a number in one place that has to agree with a
// count somewhere else. The count is derivable, so the agreement is
// checkable.
//
// # The rule, and why it is only about rows that span
//
// A row containing a colspan is a row that wants the rest of the table:
// that is what the attribute is for. So its cells have to add up to the
// column count exactly. Rows with no colspan are left alone, because a
// template puts cells behind conditionals and a literal count of them
// is not the number of cells a render produces.
//
// Tables with no column headers are skipped for the same reason they
// have none: they are key-and-value lists, where every row is a
// <th scope="row"> and a value, and there is no header count to agree
// with.
func TestASpanningCellSpansTheWholeRow(t *testing.T) {
	var (
		tableBlock = regexp.MustCompile(`(?s)<table[^>]*>.*?</table>`)
		rowBlock   = regexp.MustCompile(`(?s)<tr[^>]*>.*?</tr>`)
		columnHead = regexp.MustCompile(`<th[^>]*\bscope="col"`)
		anyCell    = regexp.MustCompile(`<t[hd]\b`)
		spanAttr   = regexp.MustCompile(`\bcolspan="(\d+)"`)
	)

	var checked int
	err := fs.WalkDir(templateFS, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".html" {
			return err
		}
		source, err := fs.ReadFile(templateFS, name)
		if err != nil {
			return err
		}
		for _, table := range tableBlock.FindAllString(string(source), -1) {
			columns := len(columnHead.FindAllString(table, -1))
			if columns == 0 {
				continue
			}
			for _, row := range rowBlock.FindAllString(table, -1) {
				spans := spanAttr.FindAllStringSubmatch(row, -1)
				if len(spans) == 0 {
					continue
				}
				checked++

				// Every cell counts once, and a spanning one counts for
				// what it claims instead.
				total := len(anyCell.FindAllString(row, -1)) - len(spans)
				for _, s := range spans {
					n, convErr := strconv.Atoi(s[1])
					if convErr != nil {
						t.Errorf("%s: colspan=%q is not a number", name, s[1])
						continue
					}
					total += n
				}
				if total != columns {
					t.Errorf(`%s: a row spans %d columns in a table that has %d.

%s
A colspan is how a row says "the rest of this table is mine", so it has
to add up to the header count. Short, and the row stops before the last
column; long, and the browser widens the table for a row nobody looks
at.`, name, total, columns, strings.TrimSpace(row))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no spanning rows found in any template; this test read nothing")
	}
	t.Logf("%d spanning rows checked", checked)
}

// A notice box used inside a line of text carries the inline modifier.
//
// # What the screenshot found
//
// .uyari, .hata, .bilgi and .kilit are notice *boxes*: border, padding,
// vertical margin. An inline box's padding does not contribute to the
// height of the line it sits on, so one of them used as a
// <span> inside a table cell draws outside its own line - and when the
// text inside it wraps, the two fragments each get a box and overlap
// whatever is beside and below them.
//
// Measured on the health page, after 5b's column narrowed the cells: a
// stale service's "haber alınamıyor" chip overlapped the timestamp next
// to it and the row underneath, so the one row an operator reads when
// something is wrong was the unreadable one. Nothing failed; the markup
// looked fine; only looking at the rendered page showed it.
//
// The fix is a modifier - .satir-ici - that makes the box part of its
// line and stops the text inside it wrapping at all. This is the rule
// that keeps the next inline notice from being written without it.
//
// # Why the scan is for the bare class rather than for table cells
//
// Because "inside a line of text" is not something a regular expression
// can decide, and a scan that tried would be wrong in both directions.
// A <span> with one of these classes is the reliable signal: these are
// block notices, so anybody reaching for a <span> is putting one inside
// a line. A <p> or a <div> with the same class is the intended use and
// is not matched.
func TestAnInlineNoticeCarriesTheInlineModifier(t *testing.T) {
	// The four classes, read from the stylesheet rather than listed
	// here: a fifth notice box added next to them is held to the same
	// rule without anybody coming back.
	// From disk rather than from templateFS: that embed covers
	// templates/, and the stylesheet is served from static/. Reading the
	// file the browser gets is the point - a list of class names typed
	// here would be the second definition this test exists to avoid.
	sheet, err := os.ReadFile(filepath.Join("static", "panel.css"))
	if err != nil {
		t.Fatal(err)
	}
	group := regexp.MustCompile(`(?s)\n((?:\.[a-z-]+,\n)+\.[a-z-]+) \{\n  border: 1px solid;`).
		FindStringSubmatch(string(sheet))
	if group == nil {
		t.Fatal("could not find the notice-box rule in panel.css; if it was reshaped, this " +
			"scan has to be reshaped with it - a pattern that stops matching turns this " +
			"test green by reading nothing")
	}
	var classes []string
	for _, sel := range strings.Split(group[1], ",") {
		classes = append(classes, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sel), ".")))
	}
	if len(classes) < 2 {
		t.Fatalf("read %v from the stylesheet; there are several notice classes", classes)
	}
	t.Logf("notice classes: %v", classes)

	var checked int
	err = fs.WalkDir(templateFS, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".html" {
			return err
		}
		source, err := fs.ReadFile(templateFS, name)
		if err != nil {
			return err
		}
		for _, class := range classes {
			bare := regexp.MustCompile(`<span[^>]*class="` + class + `"`)
			for _, hit := range bare.FindAllString(string(source), -1) {
				checked++
				t.Errorf(`%s: %s

A notice box inside a line of text. Its padding does not affect the
line's height, so it draws over whatever is beside or below it, and if
the text wraps the fragments overlap each other too.

Add the inline modifier: class="%s satir-ici".`, name, hit, class)
			}
		}
		// And the modifier is in use somewhere, so a scan that found
		// nothing because the templates stopped using these classes
		// fails instead of passing.
		if strings.Contains(string(source), "satir-ici") {
			checked++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no notice class appears in any template, inline or otherwise; this test read " +
			"nothing")
	}
}
