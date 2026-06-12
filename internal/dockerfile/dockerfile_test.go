package dockerfile

// Guards against reintroducing file capabilities on the shipped binaries.
//
// A `setcap cap_sys_nice+ep` on sim-web/sim-router makes them refuse to exec
// ("operation not permitted") on any host whose capability bounding set lacks
// CAP_SYS_NICE — e.g. an unprivileged LXC — which broke the image for every
// run, including those that never pass -rt-priority. The capability must be
// granted at runtime (--cap-add SYS_NICE) instead, so the binaries stay
// capability-free. This caught a real production breakage (packethacking/net-sim
// — the lab net-sim crash-looped after the setcap landed).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileHasNoSetcapOnBinaries(t *testing.T) {
	// Walk up to the repo root (where the Dockerfile lives).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var dockerfile string
	for d := wd; d != "/" && d != "."; d = filepath.Dir(d) {
		p := filepath.Join(d, "Dockerfile")
		if _, err := os.Stat(p); err == nil {
			dockerfile = p
			break
		}
	}
	if dockerfile == "" {
		t.Fatal("could not locate Dockerfile from " + wd)
	}
	b, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	// Allow the word inside a comment explaining why we don't do it; only fail
	// on an actual RUN setcap ... line.
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "setcap") {
			t.Fatalf("Dockerfile reintroduced file capabilities (%q) — these break exec on hosts lacking the cap in their bounding set; grant CAP_SYS_NICE at runtime via --cap-add instead", trimmed)
		}
	}
}
