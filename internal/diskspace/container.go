package diskspace

import (
	"os"
	"strings"
)

// Is this a container, and does it matter.
//
// # The failure this exists to name
//
// In the container deployment the state directory is a named volume
// (`state:/var/lib/crucible-analytic` in docker/compose.yml). Anything
// written outside a volume lives on the image's writable layer, and that
// layer is discarded the next time somebody pulls a new image and runs
// `docker compose up -d`.
//
// So a backup written to the wrong path inside a container is destroyed
// by an update. That is not an edge case here: it is the exact loss the
// backup was taken to insure against, produced by the feature working as
// written.
//
// # Why the warning needs both halves
//
// On an ordinary server the state directory is a plain directory on the
// root filesystem. It is not a mount point, and that is completely
// normal - it survives reboots, updates and everything else. A warning
// that fired on "not a mount point" alone would fire on every hand
// installed machine, every time, about nothing.
//
// A warning that is always on is a warning nobody reads. So the sentence
// is only true, and only shown, when both halves hold: this is a
// container *and* the path is not on a volume.
//
// *Bir uyarının görünür olması, fark edileceği anlamına gelmez.*

// InContainer reports whether this process looks like it is running in a
// container.
//
// A heuristic, and named as one. Three signals, any of which is enough:
//
//   - /.dockerenv, which Docker creates and nothing else does.
//   - a control group path naming a container runtime.
//   - a root filesystem of type overlay, which is how images are
//     assembled and is not how a real machine boots.
//
// Being wrong in either direction is survivable, which is what makes a
// heuristic acceptable here. A false negative means no warning, which is
// where every deployment stood before this. A false positive produces a
// warning only when the path is also not on a mount, and telling an
// operator "put this on its own filesystem" is bad advice at worst,
// never a broken deployment.
func InContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if body, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		for _, marker := range []string{"docker", "containerd", "kubepods", "lxc"} {
			if strings.Contains(string(body), marker) {
				return true
			}
		}
	}
	return rootIsOverlay()
}

// rootIsOverlay reads the mount table for the type of /.
//
// mountinfo rather than /proc/mounts: the field layout is fixed on the
// left and variable on the right, and the filesystem type is after the
// "-" separator, so it can be found without counting optional fields.
func rootIsOverlay() bool {
	body, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != "/" {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
			}
		}
		if sep >= 0 && sep+1 < len(fields) {
			return fields[sep+1] == "overlay"
		}
	}
	return false
}

// AtRisk reports whether what is written under path is destroyed by the
// next image update, and is the only question either of the two above is
// asked for.
func AtRisk(s Space) bool { return InContainer() && !s.Mount }
