package cli

import (
	"strings"
	"testing"

	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
)

// A row per Service with its project's files, so a cron job can run up on
// each; stopped ones say so and can be skipped.
func TestLsTable(t *testing.T) {
	c := func(project, role, svc, state, hash string) container.Summary {
		l := map[string]string{api.ProjectLabel: project, api.ConfigFilesLabel: "/srv/" + project + "/compose.yaml",
			transform.LabelRole: role, transform.LabelService: svc, revision.LabelRevision: "3", revision.LabelSpecHash: hash}
		if role == transform.RoleLock {
			l = map[string]string{api.ProjectLabel: project, transform.LabelRole: role} // not created by Compose
		}
		return container.Summary{Labels: l, State: container.ContainerState(state), Status: "Up 2 hours"}
	}
	var b strings.Builder
	if err := lsTable(&b, []container.Summary{
		c("web", transform.RoleProxy, "api", "running", ""),
		c("web", transform.RoleReplica, "api", "running", "h1"),
		c("web", transform.RoleReplica, "api", "running", "h1"),
		c("off", transform.RoleReplica, "api", "exited", "h1"),
		c("new", transform.RoleReplica, "api", "running", "h1"),
		c("new", transform.RoleReplica, "api", "running", "h2"),
		c("busy", transform.RoleReplica, "api", "running", "h1"),
		c("busy", transform.RoleLock, "", "created", ""),
		c("halted", transform.RoleReplica, "api", "exited", "h1"),
		c("halted", transform.RoleReplica, "api", "exited", "h2"),
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"PROJECT SERVICE REVISION REPLICAS STATUS CONFIG FILES",
		"busy api 3 1/1 bouncing /srv/busy/compose.yaml",
		"halted api 3 0/2 stopped /srv/halted/compose.yaml",
		"new api 3 2/2 drifted /srv/new/compose.yaml",
		"off api 3 0/1 stopped /srv/off/compose.yaml",
		"web api 3 2/2 converged /srv/web/compose.yaml",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
