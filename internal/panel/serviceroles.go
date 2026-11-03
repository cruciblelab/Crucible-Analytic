package panel

// ServiceRoles names the database roles the collector and the beacon
// connect as - which is also the name each writes its heartbeat row
// under: internal/heartbeat asks the connection for current_user, so the
// row's service is the role and nothing else.
//
// # Why the store has to be told
//
// Two readers of the heartbeat table pick a service out by name:
// Collecting (what each source gathers) and GeoRefusals (what the
// collector's country rules turned away). They used two constants,
// "collector" and "beacon". The second was never a role: the beacon
// connects as beacon_writer on every deployment install.sh makes, so
// Collecting has never once read the beacon's profile, and every
// sentence the breakdown page builds from it fell back to the
// no-answer path. Measured: a collector row and a beacon_writer row, both
// "tam" - Collecting{Collector:tam Beacon:""}.
//
// The names a deployment uses are in panel.toml's [roles] - the setup
// checks already read them from there - so the panel's main hands them
// on; a section left empty keeps the defaults, which are the names
// install.sh creates and panel.example.toml writes.
type ServiceRoles struct {
	Collector string
	Beacon    string
}

// DefaultServiceRoles are the roles install.sh creates.
var DefaultServiceRoles = ServiceRoles{Collector: "collector", Beacon: "beacon_writer"}

// SetServiceRoles tells the store which roles the collector and the
// beacon connect as. An empty name keeps the default. Called once, from
// the panel's main, before anything serves.
func (s *Store) SetServiceRoles(r ServiceRoles) {
	s.roles = r
}

// serviceRoles is the names in force, defaults filled in.
func (s *Store) serviceRoles() ServiceRoles {
	r := s.roles
	if r.Collector == "" {
		r.Collector = DefaultServiceRoles.Collector
	}
	if r.Beacon == "" {
		r.Beacon = DefaultServiceRoles.Beacon
	}
	return r
}
