package washhouse_test

// The core packages hold washhouse's logic and must be testable with every
// outside system faked: no filesystem, no network, no broker. This test turns
// that into a build failure. Add each core package to `core` as it is created
// (PLAN.md: vessel, profile, estimate, thermal, meter, draw, engine).
//
// Two checks, as in tanktemp:
//   - direct imports: the core must not reach the outside world itself.
//     Checked directly, not transitively, because fmt and time import os.
//   - transitive deps: the core must not depend, by any path, on the code
//     that does reach it (MQTT, HTTP, Influx).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/sweeney/washhouse"

// core lists the packages that must stay pure. A listed package that doesn't
// exist fails the test, so the list can't silently go stale.
var core = []string{
	"internal/behaviour", // decodes the behaviour document; reads no files
	"internal/draw",
	"internal/engine",
	"internal/estimate",
	"internal/meter",
	"internal/profile",
	"internal/publish", // builds messages; the service (phase 5) sends them
	"internal/tank",
	"internal/thermal",
	"internal/vessel",
}

var forbiddenDirect = []string{
	"io/fs", "io/ioutil", "net", "net/http", "os", "os/exec", "os/signal",
	"path/filepath", "syscall",
}

var forbiddenDeps = []string{
	module + "/internal/ingest",
	module + "/internal/replay", // reads files; the core takes decoded messages
	module + "/internal/influx",
	module + "/internal/httpapi",
	"github.com/eclipse/paho.mqtt.golang",
	"github.com/sweeney/common/mqttpub",
}

func goList(t *testing.T, format, pkg string, deps bool) []string {
	t.Helper()
	args := []string{"list", "-f", format}
	if deps {
		args = append(args, "-deps")
	}
	out, err := exec.Command("go", append(args, pkg)...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// readSources reads every Go file in dir, so go test's cache sees the files
// `go list` reads and an edit adding a forbidden import can't hit a cached pass.
func readSources(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			if _, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCorePackagesAreIsolated(t *testing.T) {
	for _, p := range core {
		pkg := module + "/" + p
		t.Run(p, func(t *testing.T) {
			readSources(t, p)
			for _, imp := range goList(t, `{{join .Imports " "}}`, pkg, false) {
				for _, bad := range forbiddenDirect {
					if imp == bad {
						t.Errorf("imports %s directly; reach the outside world through an interface instead", imp)
					}
				}
			}
			for _, dep := range goList(t, `{{.ImportPath}}`, pkg, true) {
				for _, bad := range forbiddenDeps {
					if dep == bad || strings.HasPrefix(dep, bad+"/") {
						t.Errorf("depends on %s", dep)
					}
				}
			}
		})
	}
}
