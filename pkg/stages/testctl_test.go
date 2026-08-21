package stages_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"kubenest.io/cli/pkg/stages"
)

// testController is a minimal Controller: enough to drive the engine without
// an install or an upgrade behind it, which is the point of the engine being
// its own package.
type testController struct {
	id      string
	journal *stages.Journal
	emitter stages.Emitter
	total   time.Duration
	out     io.Writer
}

func (c *testController) RunID() string            { return c.id }
func (c *testController) Journal() *stages.Journal { return c.journal }
func (c *testController) Emitter() stages.Emitter  { return c.emitter }
func (c *testController) BundleVersion() string    { return "1.0" }
func (c *testController) TotalDeadline() (time.Duration, error) {
	if c.total == 0 {
		return 30 * time.Minute, nil
	}
	return c.total, nil
}
func (c *testController) ResumeAdvice() string { return "" }

func (c *testController) Exits() []string {
	return []string{
		"resume     fix what the error names, then run the identical command again\n             (completed stages are skipped)",
		"start over kubenest platform uninstall --confirm",
	}
}

func (c *testController) Logf(format string, args ...any) {
	if c.out != nil {
		fmt.Fprintf(c.out, format+"\n", args...)
	}
}

// The thirteen install stage names, used by the engine tests as a realistic
// sequence. The engine itself knows none of them.
var stageNames = []string{
	"preflight", "register", "k3s-server", "k3s-agents", "platform-networking",
	"platform-certs", "platform-storage", "platform-backup", "platform-day2",
	"kubenest-agent", "profiles", "record", "verify",
}

const (
	stagePreflight  = "preflight"
	stageRegister   = "register"
	stageK3sServer  = "k3s-server"
	stageK3sAgents  = "k3s-agents"
	stageNetworking = "platform-networking"
	stageCerts      = "platform-certs"
	stageStorage    = "platform-storage"
	stageBackup     = "platform-backup"
	stageDay2       = "platform-day2"
	stageAgent      = "kubenest-agent"
	stageProfiles   = "profiles"
	stageRecord     = "record"
	stageVerify     = "verify"
)

func identity(cluster string, fields map[string]string) stages.Identity {
	return stages.Identity{Kind: "install", Cluster: cluster, Fields: fields}
}

func newController(t *testing.T, rec stages.Emitter, journalPath string, id stages.Identity) *testController {
	t.Helper()
	j, err := stages.OpenJournal(journalPath, id)
	if err != nil {
		t.Fatal(err)
	}
	return &testController{id: "run-1", journal: j, emitter: rec, out: io.Discard}
}

var _ = context.Background
