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
// each; stopped ones say so and can be skipped. Only containers Compose
// created as a Service's replicas count.
func TestLsTable(t *testing.T) {
	c := func(project, role, svc, state, hash string) container.Summary {
		l := map[string]string{api.ProjectLabel: project, api.ConfigFilesLabel: "/srv/" + project + "/compose.yaml",
			transform.LabelRole: role, transform.LabelService: svc, revision.LabelRevision: "3", revision.LabelSpecHash: hash}
		if project == "web" {
			l[transform.LabelVersion], l[transform.LabelFormat] = "v1.2.0", "1.0"
		}
		if role == transform.RoleReplica {
			l[api.ServiceLabel] = transform.AppName(svc)
		}
		if role == transform.RoleLock {
			l = map[string]string{api.ProjectLabel: project, transform.LabelRole: role} // not created by Compose
		}
		return container.Summary{Labels: l, State: container.ContainerState(state), Status: "Up 2 hours"}
	}
	// Bouncer's labels on another service's container: not a replica.
	impostor := c("web", transform.RoleReplica, "api", "exited", "h9")
	impostor.Labels[api.ServiceLabel] = "worker"
	var b strings.Builder
	if err := lsTable(&b, []container.Summary{
		c("web", transform.RoleProxy, "api", "running", ""),
		c("web", transform.RoleReplica, "api", "running", "h1"),
		c("web", transform.RoleReplica, "api", "running", "h1"),
		impostor,
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
		"PROJECT SERVICE REVISION REPLICAS STATUS VERSION FORMAT CONFIG FILES",
		"busy api 3 1/1 bouncing - - /srv/busy/compose.yaml",
		"halted api 3 0/2 stopped - - /srv/halted/compose.yaml",
		"new api 3 2/2 drifted - - /srv/new/compose.yaml",
		"off api 3 0/1 stopped - - /srv/off/compose.yaml",
		"web api 3 2/2 converged v1.2.0 1.0 /srv/web/compose.yaml",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLsHints(t *testing.T) {
	c := func(project, format string) container.Summary {
		l := map[string]string{api.ProjectLabel: project, transform.LabelRole: transform.RoleReplica,
			transform.LabelService: "api", api.ServiceLabel: "api-app"}
		if format != "" {
			l[transform.LabelFormat] = format
		}
		return container.Summary{Labels: l}
	}
	got := lsHints([]container.Summary{c("web", "1.0"), c("old", ""), c("old", "1.0"), c("minor", "1.4"), c("major", "2.0"), c("odd", "x")})
	want := []string{
		`project major: format 2.0 from bouncer  is newer than this CLI (dev) can safely change; upgrade docker-bouncer, or pass --ignore-format`,
		`project minor: deployed with a newer bouncer (, format 1.4); this CLI writes 1.0`,
		`project odd: format "x" from bouncer  is not one this CLI understands; upgrade docker-bouncer, or pass --ignore-format`,
		"project old is at format none, this CLI writes 1.0; run docker bouncer migrate old",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
