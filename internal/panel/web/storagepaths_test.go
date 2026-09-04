package web

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Every directory the panel is configured with is one it measures.
//
// # The defect this exists against
//
// The disk section could have been handed two paths written into a call
// site. That is correct on the day it is written and silently wrong
// afterwards: a third directory added to the config is a disk nobody
// watches, and the whole section exists because a disk filling up stops
// the collector, which is in front of the customer's website.
//
// So the fields are found by walking the Config with reflection rather
// than by listing them. A path added to that struct is in scope the
// moment it exists, which is the property a list cannot have.
//
// # What counts as a path
//
// A string field whose name ends in Dir or Path. That is this project's
// own convention and every field here follows it - and if a future field
// breaks it, the failure message says what the convention is rather than
// only that something is missing.

// pathFields walks a struct for fields that name a filesystem path.
func pathFields(t *testing.T, v reflect.Value, prefix string, into map[string]string) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name := prefix + f.Name
		switch f.Type.Kind() {
		case reflect.Struct:
			pathFields(t, v.Field(i), name+".", into)
		case reflect.String:
			if strings.HasSuffix(f.Name, "Dir") || strings.HasSuffix(f.Name, "Path") {
				into[name] = v.Field(i).String()
			}
		}
	}
}

// TestEveryConfiguredDirectoryIsMeasured.
func TestEveryConfiguredDirectoryIsMeasured(t *testing.T) {
	// Filled in, because an empty Config produces empty paths and
	// StoragePaths drops those - so a test against the zero value would
	// compare nothing with nothing and pass whatever the code did.
	cfg := Config{
		BotDataPath: "/var/lib/crucible-analytic/bot-data.json",
		Logging:     logging.Config{Dir: "/var/log/crucible-analytic"},
	}

	fields := map[string]string{}
	pathFields(t, reflect.ValueOf(cfg), "", fields)
	if len(fields) == 0 {
		t.Fatal("no path-shaped field was found on web.Config, so this test examined " +
			"nothing. A path field is a string named *Dir or *Path; if that convention " +
			"has changed, this check has to change with it")
	}

	measured := map[string]bool{}
	for _, p := range cfg.StoragePaths() {
		measured[p.Path] = true
	}

	for name, value := range fields {
		if value == "" {
			t.Errorf("%s was left empty by this test, so it proves nothing about %s",
				name, name)
			continue
		}
		// Either the path itself or the directory holding it: a field
		// naming a file is measured through its directory, which is the
		// same filesystem and the thing worth printing.
		if measured[value] || measured[filepath.Dir(value)] {
			continue
		}
		t.Errorf("Config.%s names %s and StoragePaths does not measure it.\n"+
			"A configured directory nobody measures is a disk that fills with no "+
			"warning, and the warning is what the disk section is for. Add it to "+
			"Config.StoragePaths with a key, and give that key words in both "+
			"catalogues", name, value)
	}
}

// TestEveryStoragePathHasWords is the other half of the mirror in
// internal/panel/ui, checked against the real function rather than
// against a list written beside it.
func TestEveryStoragePathHasWords(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BotDataPath: "/var/lib/crucible-analytic/bot-data.json",
		Logging:     logging.Config{Dir: "/var/log/crucible-analytic"},
	}
	paths := cfg.StoragePaths()
	if len(paths) == 0 {
		t.Fatal("StoragePaths returned nothing for a filled-in config")
	}
	for _, lang := range catalogs.Languages() {
		for _, p := range paths {
			key := "saglik.disk.yol." + p.Key
			if !lang.Has(key) {
				t.Errorf("%s: %s has no label, so the disk section would print a raw "+
					"identifier where a directory's name belongs", lang.Code, key)
			}
		}
	}
}

// TestAnUnsetDirectoryIsNotMeasured.
//
// logging.dir empty means file logging is off, which is a supported
// deployment. Measuring "" would render as a filesystem that could not
// be read - an error message about a decision somebody made on purpose.
func TestAnUnsetDirectoryIsNotMeasured(t *testing.T) {
	paths := Config{}.StoragePaths()
	for _, p := range paths {
		if p.Path == "" || p.Path == "." {
			t.Errorf("an unset config produced the path %q under key %q", p.Path, p.Key)
		}
	}
}
