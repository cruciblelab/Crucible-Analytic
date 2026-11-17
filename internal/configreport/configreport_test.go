package configreport_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/configreport"
	"github.com/cruciblelab/crucible-analytic/internal/panel/web"
)

// configTypes is each program's real configuration type - the one its
// main loads and hands to configreport.Values.
var configTypes = map[string]reflect.Type{
	configreport.Collector: reflect.TypeOf(collector.Config{}),
	configreport.Beacon:    reflect.TypeOf(beacon.Config{}),
	configreport.API:       reflect.TypeOf(api.Config{}),
	configreport.Panel:     reflect.TypeOf(web.Config{}),
}

// Every listed file key is a path in its program's config type. A key
// that does not resolve is left out by Values, and the wizard would say
// "not reported" about it forever - which is how the old list came to say
// "developer.password_hash" for a table called [developer_gate], and to
// put three of the collector's keys at the top level.
func TestEveryEntryResolves(t *testing.T) {
	for _, program := range configreport.Programs {
		if configTypes[program] == nil {
			t.Errorf("program %q has no config type in this test", program)
		}
	}
	for _, e := range configreport.Entries {
		if !configreport.Known(e.Program) {
			t.Errorf("%s.%s names a program that is not in Programs", e.Program, e.Key)
			continue
		}
		if e.Live {
			continue
		}
		typ := configTypes[e.Program]
		for _, part := range strings.Split(e.Key, ".") {
			field, ok := configreport.FieldByTag(typ, part)
			if !ok {
				t.Errorf("%s.%s: %s has no field tagged %q", e.Program, e.Key, typ, part)
				break
			}
			typ = field.Type
			for typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
		}
	}
}

// A key whose name says it carries a credential is marked secret.
//
// The list is decided by people, so this is the net under it: adding
// "privacy.hash_key" or "analytics_api_token" without the mark is refused
// here rather than written into a table every service can read.
var secretLooking = regexp.MustCompile(`(?i)(dsn|password|hash|token|secret)|(^|[._])key($|[._])`)

func TestASecretLookingKeyIsMarkedSecret(t *testing.T) {
	for _, e := range configreport.Entries {
		if secretLooking.MatchString(e.Key) && !e.Secret {
			t.Errorf("%s.%s looks like a credential and is not marked Secret", e.Program, e.Key)
		}
	}
}

// fill sets every string field reachable in v to a value naming its own
// TOML path, and every string slice to one such element.
func fill(v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() && v.CanSet() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		if !v.IsNil() {
			fill(v.Elem(), path)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("toml"), ",")[0]
			if name == "" || name == "-" {
				name = f.Name
			}
			child := name
			if path != "" {
				child = path + "." + name
			}
			fill(v.Field(i), child)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString("SENTINEL<" + path + ">")
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String && v.CanSet() {
			v.Set(reflect.ValueOf([]string{"SENTINEL<" + path + ">"}).Convert(v.Type()))
		}
	}
}

// What Values reports is exactly the program's non-secret file keys, each
// read from its own field - and no value anywhere in the report carries
// what a secret field held.
//
// Every string field of the real config type is set to a value naming
// its path, so a report that read the wrong field, or any secret one,
// says so in its value.
func TestValuesReportExactlyTheNonSecretKeys(t *testing.T) {
	for program, typ := range configTypes {
		cfg := reflect.New(typ)
		fill(cfg, "")
		got := configreport.Values(program, cfg.Interface())

		want := map[string]bool{}
		var secrets []string
		for _, e := range configreport.For(program) {
			switch {
			case e.Secret:
				secrets = append(secrets, "SENTINEL<"+e.Key+">")
			case !e.Live:
				want[e.Key] = true
			}
		}
		for key, value := range got {
			if !want[key] {
				t.Errorf("%s reported %q, which is not one of its non-secret file keys", program, key)
			}
			for _, secret := range secrets {
				if strings.Contains(value, secret) {
					t.Errorf("%s reported %s = %q, which carries a secret field's value", program, key, value)
				}
			}
		}
		for key := range want {
			value, ok := got[key]
			if !ok {
				t.Errorf("%s did not report %s", program, key)
				continue
			}
			// A field whose type is not a string - mode, a bool - keeps
			// its zero value; every string-shaped one names its path.
			if strings.HasPrefix(value, "SENTINEL<") && value != "SENTINEL<"+key+">" {
				t.Errorf("%s reported %s = %q: read from the wrong field", program, key, value)
			}
		}
		// And by value too: the report passed through a struct, so the
		// pointer and the value must say the same.
		if byValue := configreport.Values(program, cfg.Elem().Interface()); !reflect.DeepEqual(byValue, got) {
			t.Errorf("%s: Values by value = %v, by pointer = %v", program, byValue, got)
		}
	}
}

// Bound cuts on a rune boundary and says it cut.
func TestBoundCutsOnARune(t *testing.T) {
	// One byte, then two-byte characters: the limit lands in the middle of
	// one. ş alone would put it exactly on a boundary, and a cut that
	// ignored characters would pass (it did, under mutation).
	long := "a" + strings.Repeat("ş", configreport.MaxValueBytes)
	got := configreport.Bound(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a cut value does not say it was cut")
	}
	if !utf8.ValidString(got) {
		t.Errorf("Bound cut inside a character")
	}
	if len(got) > configreport.MaxValueBytes+len("…") {
		t.Errorf("Bound left %d bytes, want at most %d", len(got), configreport.MaxValueBytes+len("…"))
	}
	if short := "127.0.0.1:8081"; configreport.Bound(short) != short {
		t.Errorf("Bound changed a short value")
	}
	if exact := strings.Repeat("a", configreport.MaxValueBytes); configreport.Bound(exact) != exact {
		t.Errorf("Bound cut a value of exactly the limit")
	}
}
