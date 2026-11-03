package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// DeploymentQuerier is the read-only access the deployment-wide handlers
// need: questions about every site at once, which only a wildcard token
// may ask (see handleBlocklistPreview).
type DeploymentQuerier interface {
	BlocklistPreview(ctx context.Context, countries []string, asns []int) (BlocklistPreview, error)
}

// registerDeploymentRoutes adds the routes whose answer is about every
// site at once.
func (s *Server) registerDeploymentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/blocklist/preview", s.handleBlocklistPreview)
}

// handleBlocklistPreview answers "what would these country and ASN rules
// have refused in the last day" (B3f, catalog #12).
//
//	GET /api/v1/blocklist/preview?country=RU&country=CN&asn=64512
//
// # Wildcard tokens only
//
// The blocklist is deployment-wide, so the answer counts every site's
// addresses. A token scoped to some sites is refused with 403 rather than
// answered for its own sites: the question being asked is about the whole
// deployment, and a per-site answer to it would be a different number
// under the same name. The panel's token is the wildcard one, and on the
// panel side only somebody who may change the blocklist - the operator,
// or an account that owns every site (H10) - is shown it.
//
// # Parameters
//
// country is a two-letter code, any case; asn a positive number that fits
// the column. At most MaxBlocklistPreviewRules rules together. Duplicates
// are asked about once. No rule at all is a valid question: the three
// totals alone say whether the lookup places anything.
func (s *Server) handleBlocklistPreview(w http.ResponseWriter, r *http.Request) {
	tok, _ := r.Context().Value(tokenContextKey{}).(Token)
	if !tok.CanRead(WildcardSite) {
		writeError(w, http.StatusForbidden, fmt.Sprintf(
			"token %q is not a wildcard token; the blocklist preview counts every site", tok.Name))
		return
	}
	countries, asns, err := parseBlocklistRules(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.Store.BlocklistPreview(r.Context(), countries, asns)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// parseBlocklistRules reads and checks the rules asked about.
//
// The same shape the panel's validators accept for the settings
// themselves (panel.checkCountryCodes, checkASNList), so a value the
// panel would store is a value this answers for - plus the column's own
// ceiling, which the setting's validator does not need: a stored ASN
// above it simply never matches, a query parameter above it would fail
// the statement.
func parseBlocklistRules(q map[string][]string) ([]string, []int, error) {
	countries := []string{}
	seenCountry := map[string]bool{}
	for _, raw := range q["country"] {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil, nil, fmt.Errorf("country %.10q is not a two-letter code", raw)
		}
		if !seenCountry[code] {
			seenCountry[code] = true
			countries = append(countries, code)
		}
	}
	asns := []int{}
	seenASN := map[int]bool{}
	for _, raw := range q["asn"] {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n <= 0 || n > math.MaxInt32 {
			return nil, nil, fmt.Errorf("asn %.12q is not a positive number the table can hold", raw)
		}
		if !seenASN[n] {
			seenASN[n] = true
			asns = append(asns, n)
		}
	}
	if len(countries)+len(asns) > MaxBlocklistPreviewRules {
		return nil, nil, fmt.Errorf("at most %d rules per preview, got %d",
			MaxBlocklistPreviewRules, len(countries)+len(asns))
	}
	return countries, asns, nil
}
