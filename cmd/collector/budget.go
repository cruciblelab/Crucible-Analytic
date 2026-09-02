package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/memlimit"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
)

// # What this file is preventing
//
// The collector stands in front of the customer's website. When it dies
// the site goes with it, so its failures are the customer's failures,
// and the one measured in A2 is the worst kind: a deployment picks the
// largest IP-intelligence profile on a machine that cannot hold it, runs
// perfectly for hours, and is killed by the kernel during a dataset
// refresh. Nothing warns anybody. The site goes down at the refresh
// interval, which is once a day, at whatever hour the process happened
// to start.
//
// Measured rather than assumed: internal/profile's floors come from
// running each profile's refresh inside a container five times at each
// size and keeping the smallest size where all five lived. The two rows
// that matter are the ones that survived *sometimes* - 128m one time in
// five, 256m two times in five - because a single lucky run is what
// makes a budget look fine.
//
// # Why it refuses in one case and only warns in the other
//
// The customer's instruction, and it is the right one: block what would
// actually crash and nothing else. "Kaynak yetersizse ... biz
// engellemezsek patlarsa bize sorarlar. Onun dışında bir şeye
// zorlamayalım, en iyi serbestliği verelim."
//
// So the split follows what the ceiling is worth:
//
//   - A cgroup limit is enforced. The kernel will kill this process for
//     exceeding it - not might, will - and the number does not move. A
//     configuration that does not fit under it has already failed;
//     starting anyway only chooses when.
//   - MemAvailable is an estimate, on a machine where TimescaleDB is
//     sized to take most of the memory and where a backup can move the
//     figure by hundreds of megabytes between two readings. Refusing on
//     it would turn a busy minute into an outage, and that outage would
//     be ours rather than the machine's.
//
// Unknown is not a refusal either, for the reason internal/memlimit
// gives: an unreadable /proc must not become a down site.

// verdict is the decision, separated from the reporting so it can be
// tested without a filesystem.
//
// Split out after the first version called memlimit.Detect() inline,
// which made every interesting case - a 256 MB container, an unreadable
// /proc - reachable only by running the tests on a machine that happened
// to have one.
type verdict struct {
	// start is whether the process may carry on.
	start bool
	// why is empty when the profile fits, and otherwise the sentence an
	// operator reads first.
	why string
	// suggestion names the largest profile that would fit here. Filled
	// in whenever why is, because "you are short of memory" is not an
	// instruction and this may be the only warning anybody gets.
	suggestion string
}

// budgetVerdict decides, from numbers alone.
func budgetVerdict(prof profile.Profile, rateStore uint64, bounded bool, ceiling memlimit.Limit) verdict {
	ok, why := prof.Fits(ceiling, rateStore, bounded)
	if ok {
		return verdict{start: true}
	}
	return verdict{
		start:      !ceiling.Exact(),
		why:        why,
		suggestion: largestThatFits(ceiling, rateStore, bounded),
	}
}

// checkMemoryBudget reports whether this process should carry on, and
// says what it found either way.
func checkMemoryBudget(cfg *collector.Config, logger *slog.Logger) bool {
	prof, named := cfg.Profile()
	if !named {
		// Cannot happen with today's three levels, which are
		// exhaustive. If a fourth is added and not given a profile, the
		// honest thing is to say so and carry on rather than to invent
		// a budget for it.
		logger.Warn("this configuration does not match any known resource profile, "+
			"so its memory budget was not checked",
			"level", cfg.ProfileLevel())
		return true
	}

	rateStore, bounded := cfg.RateStoreBound()
	ceiling := memlimit.Detect()

	logger.Info("resource profile",
		"profile", prof.ID,
		"label", prof.Label,
		"needs_mb", prof.Needs(rateStore)/(1<<20),
		"ceiling_mb", ceiling.Bytes/(1<<20),
		"ceiling_from", string(ceiling.From),
		"rate_store_bounded", bounded)

	v := budgetVerdict(prof, rateStore, bounded, ceiling)
	switch {
	case v.why == "":
		return true

	case v.start:
		logger.Warn("this profile probably does not fit on this machine, and the "+
			"collector is starting anyway because the figure it was compared against "+
			"is an estimate rather than an enforced limit",
			"why", v.why, "suggestion", v.suggestion)
		return true

	default:
		// Refused, and on stderr as well as in the log: this happens at
		// startup, usually with a person watching, and the log may be
		// going somewhere they are not.
		logger.Error("refusing to start: this profile does not fit under this "+
			"container's memory limit", "why", v.why)
		fmt.Fprintf(os.Stderr, "collector: %s\n", v.why)
		fmt.Fprintf(os.Stderr, "collector: %s\n", v.suggestion)
		return false
	}
}

// largestThatFits turns a refusal into an instruction.
//
// A message that says only "does not fit" leaves the reader to work out
// which of three profiles to try, and the code already knows. When none
// of them fits, saying so is better than naming the smallest and being
// wrong twice.
func largestThatFits(ceiling memlimit.Limit, rateStore uint64, bounded bool) string {
	best := ""
	for _, p := range profile.All() {
		if ok, _ := p.Fits(ceiling, rateStore, bounded); ok {
			best = p.ID
		}
	}
	if best == "" {
		return "Bu bellekle hiçbir profil çalışmıyor; belleği artırmak gerekiyor."
	}
	return fmt.Sprintf("Bu bellekte çalışacak en büyük profil: %q "+
		"(asn_lookup ayarlarını ona göre değiştirin).", best)
}

// collectorProfileID is what this process reports as its profile.
//
// Empty when nothing matches, rather than a made-up name: the panel
// renders an empty profile as nothing at all, which is the truthful
// rendering of "this build does not know what it is running".
func collectorProfileID(cfg *collector.Config) string {
	if p, ok := cfg.Profile(); ok {
		return p.ID
	}
	return ""
}
