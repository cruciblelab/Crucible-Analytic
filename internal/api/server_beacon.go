package api

import (
	"context"
	"net/http"
	"time"
)

// BeaconQuerier is the read-only access the beacon handlers need.
type BeaconQuerier interface {
	BeaconSites(ctx context.Context) ([]string, error)
	BeaconSummary(ctx context.Context, siteID string, from, to time.Time, bots BotFilter, campaign campaignFilter) (BeaconSummary, error)
	BeaconTimeseries(ctx context.Context, siteID string, from, to time.Time, interval string, bots BotFilter, campaign campaignFilter) ([]BeaconBucket, error)
	BeaconPages(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconTitles(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconUTMSources(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconUTMMediums(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconUTMCampaigns(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconUTMTerms(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconUTMContents(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconRefs(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconClickSources(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconEntryPages(ctx context.Context, siteID string, p beaconParams) ([]SessionPathStat, int, error)
	BeaconExitPages(ctx context.Context, siteID string, p beaconParams) ([]SessionPathStat, int, error)
	BeaconReferrers(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconCampaigns(ctx context.Context, siteID string, p beaconParams) ([]CampaignStat, int, error)
	BeaconBrowsers(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconOperatingSystems(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconDevices(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconLanguages(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconCountries(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error)
	BeaconEvents(ctx context.Context, siteID string, p beaconParams) ([]EventStat, int, error)
	BeaconRaw(ctx context.Context, siteID string, p beaconParams) ([]BeaconEvent, int, error)
}

// CrossoverQuerier is the read-only access the cross-source handlers
// need. Separate from BeaconQuerier because these read *both* tables -
// the whole point of them - so a reader of the interface list can see at
// a glance which endpoints depend on the collector also running.
type CrossoverQuerier interface {
	CrossoverSummary(ctx context.Context, siteID string, from, to time.Time) (CrossoverSummary, error)
	SilentIPs(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]IPStat, int, error)
	JSBots(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]JSBot, int, error)
}

// registerBeaconRoutes adds the client-side and cross-source endpoints.
//
// They live under their own path segments rather than alongside the
// collector's because the two sources measure different populations, and
// the URL is the only place a caller reliably reads that: /countries
// counts addresses the collector saw, /beacon/countries counts people
// who rendered a page, and quietly serving both under one name would
// invite a panel to chart them against each other.
func (s *Server) registerBeaconRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/beacon/sites", s.handleBeaconSites)

	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/summary", s.siteHandler(s.handleBeaconSummary))
	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/timeseries", s.siteHandler(s.handleBeaconTimeseries))
	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/entry-pages", s.siteHandler(s.handleBeaconEntryPages))
	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/exit-pages", s.siteHandler(s.handleBeaconExitPages))
	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/campaigns", s.siteHandler(s.handleBeaconCampaigns))
	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/events", s.siteHandler(s.handleBeaconEvents))
	mux.HandleFunc("GET /api/v1/sites/{site}/beacon/raw", s.siteHandler(s.handleBeaconRaw))

	// The plain one-column breakdowns differ only in which column they
	// group by and what the rows are called, so they share a handler
	// rather than repeating the same twelve lines seven times.
	//
	// The queries are named as *method expressions*, not as methods
	// bound to s.Store. That matters: a bound method value would capture
	// whatever Store happened to be set when Handler() was called, so
	// these seven routes would keep using a stale store if it were ever
	// replaced afterwards while the other twenty read the current one -
	// an inconsistency that would be very hard to see in a test failure.
	// A method expression takes its receiver as an argument, which
	// beaconGroupHandler supplies per request.
	for path, group := range map[string]struct {
		key   string
		query beaconGroupQuery
	}{
		"pages":             {"pages", BeaconQuerier.BeaconPages},
		"referrers":         {"referrers", BeaconQuerier.BeaconReferrers},
		"browsers":          {"browsers", BeaconQuerier.BeaconBrowsers},
		"operating-systems": {"operating_systems", BeaconQuerier.BeaconOperatingSystems},
		"devices":           {"devices", BeaconQuerier.BeaconDevices},
		"languages":         {"languages", BeaconQuerier.BeaconLanguages},
		"countries":         {"countries", BeaconQuerier.BeaconCountries},
		"titles":            {"titles", BeaconQuerier.BeaconTitles},
		"utm-sources":       {"utm_sources", BeaconQuerier.BeaconUTMSources},
		"utm-mediums":       {"utm_mediums", BeaconQuerier.BeaconUTMMediums},
		"utm-campaigns":     {"utm_campaigns", BeaconQuerier.BeaconUTMCampaigns},
		"utm-terms":         {"utm_terms", BeaconQuerier.BeaconUTMTerms},
		"utm-contents":      {"utm_contents", BeaconQuerier.BeaconUTMContents},
		"refs":              {"refs", BeaconQuerier.BeaconRefs},
		"click-sources":     {"click_sources", BeaconQuerier.BeaconClickSources},
	} {
		mux.HandleFunc("GET /api/v1/sites/{site}/beacon/"+path, s.siteHandler(s.beaconGroupHandler(group.key, group.query)))
	}

	mux.HandleFunc("GET /api/v1/sites/{site}/crossover/summary", s.siteHandler(s.handleCrossoverSummary))
	mux.HandleFunc("GET /api/v1/sites/{site}/crossover/silent-ips", s.siteHandler(s.handleSilentIPs))
	mux.HandleFunc("GET /api/v1/sites/{site}/crossover/js-bots", s.siteHandler(s.handleJSBots))
}

// beaconGroupQuery is the shape of a one-column breakdown, written as a
// method expression so the receiver is supplied per request rather than
// captured at route-registration time.
type beaconGroupQuery = func(BeaconQuerier, context.Context, string, beaconParams) ([]BeaconGroupStat, int, error)

// parseBeaconParams reads the parameters the beacon list endpoints
// share, writing the 400 itself and reporting ok=false if any is
// invalid.
func (s *Server) parseBeaconParams(w http.ResponseWriter, r *http.Request) (beaconParams, bool) {
	q := r.URL.Query()
	if err := rejectBotScoreMin(q); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return beaconParams{}, false
	}
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return beaconParams{}, false
	}
	limit, err := ParseLimit(q, 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return beaconParams{}, false
	}
	offset, err := ParseOffset(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return beaconParams{}, false
	}
	bots, err := ParseBotFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return beaconParams{}, false
	}
	campaign, err := ParseCampaignFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return beaconParams{}, false
	}
	return beaconParams{from: from, to: to, limit: limit, offset: offset, bots: bots, campaign: campaign}, true
}

func (s *Server) handleBeaconSites(w http.ResponseWriter, r *http.Request) {
	tok, _ := r.Context().Value(tokenContextKey{}).(Token)

	// Same rule as handleSites: only a wildcard token asks the database
	// what exists, and an enumerated one is never told about sites it
	// cannot read.
	var sites []string
	if tok.CanRead(WildcardSite) {
		all, err := s.Store.BeaconSites(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		sites = all
	} else {
		sites = tok.Sites
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

func (s *Server) handleBeaconSummary(w http.ResponseWriter, r *http.Request, site string) {
	q := r.URL.Query()
	if err := rejectBotScoreMin(q); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bots, err := ParseBotFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	campaign, err := ParseCampaignFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := s.Store.BeaconSummary(r.Context(), site, from, to, bots, campaign)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleBeaconTimeseries(w http.ResponseWriter, r *http.Request, site string) {
	q := r.URL.Query()
	if err := rejectBotScoreMin(q); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	interval, err := ParseInterval(q.Get("interval"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bots, err := ParseBotFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	campaign, err := ParseCampaignFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	buckets, err := s.Store.BeaconTimeseries(r.Context(), site, from, to, interval, bots, campaign)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id":  site,
		"from":     from,
		"to":       to,
		"interval": interval,
		"bots":     string(bots),
		"buckets":  buckets,
	})
}

// beaconGroupHandler builds the handler shared by every one-column
// breakdown.
func (s *Server) beaconGroupHandler(key string, query beaconGroupQuery) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, site string) {
		p, ok := s.parseBeaconParams(w, r)
		if !ok {
			return
		}
		stats, total, err := query(s.Store, r.Context(), site, p)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, p.envelope(site, key, stats, total))
	}
}

func (s *Server) handleBeaconEntryPages(w http.ResponseWriter, r *http.Request, site string) {
	s.sessionPages(w, r, site, "entry_pages", s.Store.BeaconEntryPages)
}

func (s *Server) handleBeaconExitPages(w http.ResponseWriter, r *http.Request, site string) {
	s.sessionPages(w, r, site, "exit_pages", s.Store.BeaconExitPages)
}

func (s *Server) sessionPages(w http.ResponseWriter, r *http.Request, site, key string, query func(context.Context, string, beaconParams) ([]SessionPathStat, int, error)) {
	p, ok := s.parseBeaconParams(w, r)
	if !ok {
		return
	}
	stats, total, err := query(r.Context(), site, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, key, stats, total))
}

func (s *Server) handleBeaconCampaigns(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseBeaconParams(w, r)
	if !ok {
		return
	}
	stats, total, err := s.Store.BeaconCampaigns(r.Context(), site, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "campaigns", stats, total))
}

func (s *Server) handleBeaconEvents(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseBeaconParams(w, r)
	if !ok {
		return
	}
	stats, total, err := s.Store.BeaconEvents(r.Context(), site, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "events", stats, total))
}

func (s *Server) handleBeaconRaw(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseBeaconParams(w, r)
	if !ok {
		return
	}
	events, total, err := s.Store.BeaconRaw(r.Context(), site, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "events", events, total))
}

func (s *Server) handleCrossoverSummary(w http.ResponseWriter, r *http.Request, site string) {
	from, to, err := ParseRange(r.URL.Query(), s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := s.Store.CrossoverSummary(r.Context(), site, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleSilentIPs(w http.ResponseWriter, r *http.Request, site string) {
	// listParams, not beaconParams: these rows come from
	// traffic_snapshots, so the collector's own vocabulary applies.
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}
	ips, total, err := s.Store.SilentIPs(r.Context(), site, p.from, p.to, p.limit, p.offset)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "ips", ips, total))
}

func (s *Server) handleJSBots(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}
	bots, total, err := s.Store.JSBots(r.Context(), site, p.from, p.to, p.limit, p.offset, p.botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "bots", bots, total))
}
