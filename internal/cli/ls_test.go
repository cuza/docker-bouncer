package cli

import (
	"strings"
	"testing"

	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
)

// Every Compose project is listed with its files, plain-only ones included,
// so a cron job can run up on each; stopped ones say so and can be skipped.
func TestLsTable(t *testing.T) {
	c := func(project, role, svc, state, hash string) container.Summary {
		l := map[string]string{api.ProjectLabel: project, api.ConfigFilesLabel: "/srv/" + project + "/compose.yaml"}
		if role != "" {
			l[transform.LabelRole], l[transform.LabelService] = role, svc
			l[revision.LabelRevision], l[revision.LabelSpecHash] = "3", hash
		}
		if role == transform.RoleLock {
			delete(l, api.ConfigFilesLabel) // the lock is not created by Compose
		}
		return container.Summary{Labels: l, State: container.ContainerState(state), Status: "Up 2 hours"}
	}
	var b strings.Builder
	if err := lsTable(&b, []container.Summary{
		c("web", transform.RoleProxy, "api", "running", ""),
		c("web", transform.RoleReplica, "api", "running", "h1"),
		c("web", transform.RoleReplica, "api", "running", "h1"),
		c("web", "", "", "running", ""), // a plain service next to the Service
		c("eden", "", "", "running", ""),
		c("old", "", "", "exited", ""),
		c("off", transform.RoleReplica, "api", "exited", "h1"),
		c("busy", transform.RoleLock, "", "created", ""),
		c("busy", "", "", "running", ""),
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"PROJECT SERVICE REVISION REPLICAS STATUS CONFIG FILES",
		"busy - - - bouncing /srv/busy/compose.yaml",
		"eden - - - running /srv/eden/compose.yaml",
		"off api 3 0/1 stopped /srv/off/compose.yaml",
		"old - - - stopped /srv/old/compose.yaml",
		"web api 3 2/2 converged /srv/web/compose.yaml",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
