package engine

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/paths"
)

// TestKillHelper is not a test: TestKilledMidUpdateIsRepaired runs the test binary again with this
// one selected. It updates the rig the parent prepared (paths and fake GitHub passed in the
// environment), says where it is when it reaches the chosen fault point, and then waits to be
// killed.
func TestKillHelper(t *testing.T) {
	if os.Getenv("GREMLOADER_KILL_HELPER") != "1" {
		t.Skip("helper process")
	}
	stop, _ := strconv.Atoi(os.Getenv("GREMLOADER_KILL_AT"))
	r := &rig{t: t, root: os.Getenv("GREMLOADER_KILL_ROOT"), file: os.Getenv("GREMLOADER_KILL_FILE"), hooks: &fakeHooks{}}
	r.paths = paths.Paths{State: os.Getenv("GREMLOADER_KILL_STATE"), Config: os.Getenv("GREMLOADER_KILL_CONFIG")}
	r.clock = clock.NewFake(time.Now().UTC())
	e := r.engineAt(os.Getenv("GREMLOADER_KILL_API"), os.Getenv("GREMLOADER_KILL_WEB"))
	n := 0
	e.fault = func(point string) error {
		if n++; n == stop {
			fmt.Printf("stopped at %d %s\n", n, point)
			time.Sleep(time.Hour)
		}
		return nil
	}
	p := r.plan(e, Request{})
	_ = e.Apply(context.Background(), p, ApplyOptions{})
	fmt.Println("finished")
	os.Exit(0)
}

// A real process kill, not a simulated one: open handles vanish, nothing is flushed on the way
// out. Repair must still leave exactly the old or exactly the new version.
func TestKilledMidUpdateIsRepaired(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	total := countPoints(t)
	for _, k := range []int{total / 4, total / 2, total * 3 / 4, total - 2} {
		t.Run(strconv.Itoa(k), func(t *testing.T) {
			r := atV1(t)
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(self, "-test.run=^TestKillHelper$") //nolint:gosec // G204: our own test binary
			cmd.Env = append(os.Environ(),
				"GREMLOADER_KILL_HELPER=1", "GREMLOADER_KILL_AT="+strconv.Itoa(k),
				"GREMLOADER_KILL_ROOT="+r.root, "GREMLOADER_KILL_FILE="+r.file,
				"GREMLOADER_KILL_STATE="+r.paths.State, "GREMLOADER_KILL_CONFIG="+r.paths.Config,
				"GREMLOADER_KILL_API="+r.srv.APIBase(), "GREMLOADER_KILL_WEB="+r.srv.WebBase())
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			line, _ := bufio.NewReader(out).ReadString('\n')
			if !strings.HasPrefix(line, "stopped at") {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("helper said %q", line)
			}
			t.Logf("killed: %s", strings.TrimSpace(line))
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()

			for _, ri := range r.engine().Repair(context.Background()) {
				if ri.Result != ResultRepaired {
					t.Errorf("%s: repair %+v", line, ri)
				}
			}
			got := tree(t, r.dir())
			v := r.state().Packages["app"].Version
			if old, updated := maps.Equal(got, v1Tree) && v == "1.0.0", maps.Equal(got, v2Tree) && v == "2.0.0"; !old && !updated {
				t.Fatalf("%sstate says %s and the folder holds:\n%s", line, v, show(got))
			}
			t.Logf("after repair: version %s", v)
			r.assertClean()
		})
	}
}
