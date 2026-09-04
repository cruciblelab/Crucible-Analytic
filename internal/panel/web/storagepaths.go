package web

import "path/filepath"

// Which directories the disk section measures.
//
// # Derived from the configuration, not listed on the page
//
// The health page could have been given two hard-coded paths. It would
// be correct today and wrong the first time somebody adds a third: a
// configured directory nobody measures is a disk that fills without
// warning, and the warning is the entire point of the section.
//
// So the list comes off the Config, and an invariant test walks that
// struct with reflection and fails when a path-shaped field is not
// covered here. Adding a directory to the config and forgetting this
// file is the mistake being designed out.
//
// *İki listenin birbirini tutması gerekiyorsa, biri fazladır.*

// StoragePath is one configured directory, with the name the page shows
// it under.
type StoragePath struct {
	// Key names it in the message catalogue: "saglik.disk.yol." + Key.
	Key string
	// Path is the directory itself.
	Path string
}

// StoragePaths returns the directories this configuration names, in the
// order the page shows them.
//
// Empty values are dropped rather than measured. An unset log directory
// is a supported deployment - logging.dir empty means file logging is
// off - and reporting "" as a filesystem that could not be read would be
// an error message about a decision somebody made on purpose.
func (c Config) StoragePaths() []StoragePath {
	var out []StoragePath
	if c.Logging.Dir != "" {
		out = append(out, StoragePath{Key: "kayit", Path: c.Logging.Dir})
	}
	if c.BotDataPath != "" {
		// The directory, not the file. Statfs would answer for either,
		// and the answer is about the filesystem in both cases - but the
		// page prints this path, and printing a file where every other
		// row is a directory reads as a mistake.
		out = append(out, StoragePath{Key: "veri", Path: filepath.Dir(c.BotDataPath)})
	}
	return out
}
