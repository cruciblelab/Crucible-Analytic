package main

import (
	"slices"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/configreport"
)

// acceptedSites is the site list the beacon applied last, and whether it
// came from the panel's beacon.sites rather than this file's sites.
type acceptedSites struct {
	sites     []string
	fromPanel bool
}

// withoutRefused is sites less the entries SetSites would not take.
func withoutRefused(sites, refused []string) []string {
	out := make([]string, 0, len(sites))
	for _, site := range sites {
		if !slices.Contains(refused, site) {
			out = append(out, site)
		}
	}
	return out
}

// beaconConfigReport is what the beacon says about itself in its
// heartbeat: the file keys internal/configreport lists, and - once the
// settings have been applied - the sites it accepts and their source.
//
// path_prefix is the prefix in force rather than the file's value: the
// file's empty value means the default, which the server applies, and the
// wizard showed "" for a beacon mounted at /_ca.
func beaconConfigReport(cfg beacon.Config, prefixInForce string, now *acceptedSites) configreport.Report {
	values := configreport.Values(configreport.Beacon, cfg)
	values["path_prefix"] = prefixInForce
	if now != nil {
		values[configreport.KeyAcceptedSites] = strings.Join(now.sites, ", ")
		values[configreport.KeySitesSource] = configreport.SourceFile
		if now.fromPanel {
			values[configreport.KeySitesSource] = configreport.SourcePanel
		}
	}
	return configreport.Report{Program: configreport.Beacon, Values: values}
}
