package beacon

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The snippet goes out compressed to a client that accepts it, and the
// two encodings are two representations.
//
// # What this is for
//
// Nothing in this serving path compressed anything. Measured before this
// change: 16.4 KB to every client, including the ones sending
// `Accept-Encoding: gzip`, with no Content-Encoding header. README had
// claimed "2.1 KB over the wire (gzipped)" for months - the expectation
// was documented, the delivery was not, and no test asked.
//
// # The half that is easy to get wrong
//
// An entity tag identifies a representation, not a resource. If both
// encodings share one tag, a shared cache stores whichever body it saw
// first and then answers a conditional request for the *other* encoding
// with 304 - leaving that client holding bytes in an encoding it never
// asked for, which for a script means a syntax error on somebody's site.
// So the tags must differ, Vary must name Accept-Encoding, and a
// conditional request must be judged against the tag of the variant it
// is asking for. All four are asserted below; three of them would pass
// with a single shared tag.
func TestTheSnippetIsCompressedForClientsThatTakeIt(t *testing.T) {
	s, prefix := newPrivacyServer(t, "masked")
	path := prefix + "/ca.js"

	get := func(accept string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if accept != "" {
			r.Header.Set("Accept-Encoding", accept)
		}
		for _, m := range mutate {
			m(r)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}

	// ---- a client that accepts gzip ----
	zipped := get("gzip, deflate, br")
	if zipped.Code != http.StatusOK {
		t.Fatalf("the script answered %d", zipped.Code)
	}
	if got := zipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q for a client that accepts gzip.\n"+
			"Without it the body is 16.4 KB on every cold load, and nothing else in "+
			"this path compresses: not the beacon, not the collector's proxy mode, "+
			"and not the nginx configuration KURULUM describes.", got)
	}
	if got := zipped.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q; a cache that does not key on Accept-Encoding will hand "+
			"one variant to every client", got)
	}

	// The compressed body has to be both smaller and the same script.
	if zipped.Body.Len() >= len(scriptSource) {
		t.Errorf("the compressed body is %d bytes and the script is %d; nothing was "+
			"gained", zipped.Body.Len(), len(scriptSource))
	}
	zr, err := gzip.NewReader(strings.NewReader(zipped.Body.String()))
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the compressed body: %v", err)
	}
	if string(decoded) != string(scriptSource) {
		t.Errorf("the decompressed body is %d bytes and the script is %d; a client "+
			"would be running something other than the snippet",
			len(decoded), len(scriptSource))
	}

	// ---- a client that does not ----
	plain := get("")
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q for a client that asked for no encoding", got)
	}
	if plain.Body.String() != string(scriptSource) {
		t.Error("the identity body is not the script")
	}
	if got := plain.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q on the identity answer too - the resource varies whether "+
			"or not this particular answer was compressed", got)
	}

	// ---- two representations, two tags ----
	zipTag, plainTag := zipped.Header().Get("ETag"), plain.Header().Get("ETag")
	switch {
	case zipTag == "" || plainTag == "":
		t.Fatalf("a variant answered without an ETag (gzip %q, identity %q)",
			zipTag, plainTag)
	case zipTag == plainTag:
		t.Error("both encodings carry the same ETag. A shared cache stores one body " +
			"and then answers 304 to a conditional request for the other encoding, " +
			"so a client ends up with a gzip stream it will try to parse as " +
			"JavaScript. The tag identifies the representation, not the resource.")
	}

	// ---- and a conditional request is judged against its own variant ----
	//
	// The cross pair is the assertion that matters: the identity tag
	// must NOT satisfy a gzip request. A handler that compared against
	// one tag for both would answer 304 here and the client would keep
	// the wrong bytes.
	for _, tc := range []struct {
		name   string
		accept string
		tag    string
		want   int
	}{
		{"gzip asks with the gzip tag", "gzip", zipTag, http.StatusNotModified},
		{"identity asks with the identity tag", "", plainTag, http.StatusNotModified},
		{"gzip asks with the identity tag", "gzip", plainTag, http.StatusOK},
		{"identity asks with the gzip tag", "", zipTag, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := get(tc.accept, func(r *http.Request) {
				r.Header.Set("If-None-Match", tc.tag)
			})
			if got.Code != tc.want {
				t.Errorf("answered %d, want %d", got.Code, tc.want)
			}
		})
	}
}

// A client that says it does not want gzip does not get gzip.
//
// `Accept-Encoding: gzip;q=0` is the spelling for "do not send me this",
// and a substring test for "gzip" reads it as consent. Rare - and the
// rare case is the whole reason to parse rather than search: a visitor
// whose browser or proxy is telling us something specific is not a
// visitor to guess about.
func TestAClientThatRefusesGzipIsNotSentGzip(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"gzip", "gzip"},
		{"gzip, deflate", "gzip"},
		{"deflate, gzip;q=1.0", "gzip"},
		{"GZIP", "gzip"},
		{" gzip ", "gzip"},

		{"", ""},
		{"identity", ""},
		{"deflate, br", ""},
		{"gzip;q=0", ""},
		{"gzip;q=0, deflate", ""},
		{"gzip ; q=0", ""},
	} {
		t.Run(tc.header, func(t *testing.T) {
			s, prefix := newPrivacyServer(t, "masked")
			r := httptest.NewRequest(http.MethodGet, prefix+"/ca.js", nil)
			if tc.header != "" {
				r.Header.Set("Accept-Encoding", tc.header)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)

			if got := w.Header().Get("Content-Encoding"); got != tc.want {
				t.Errorf("Accept-Encoding %q -> Content-Encoding %q, want %q",
					tc.header, got, tc.want)
			}
		})
	}
}
