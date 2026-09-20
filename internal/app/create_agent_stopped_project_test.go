package app

import (
	"strings"
	"testing"

	"github.com/crevissepartners/projmux/internal/integrations/tmuxopts"
)

// stopAndEmptyProject turns a fixture Project into the exact state an operator
// reaches by deleting a Project's last Window: no Windows at all, an empty
// spec.primaryWindowRef, and no live tmux session. The registry is valid there,
// which is why the state survives long enough to be met again later.
func stopAndEmptyProject(t *testing.T, store *fakeResourceStore, tmux *fakeTmux, projectUID string) {
	t.Helper()
	mutator := store.mutator()
	for _, window := range store.registry.WindowsOf(projectUID) {
		if err := mutator.DeleteWindow(&store.registry, window.Metadata.UID); err != nil {
			t.Fatalf("empty project %s: %v", projectUID, err)
		}
	}
	project, ok := store.registry.Project(projectUID)
	if !ok {
		t.Fatalf("project %s disappeared", projectUID)
	}
	if project.Status.Session != nil {
		project.Status.Session.Live = false
	}
	if project.Spec.PrimaryWindowRef != "" {
		t.Fatalf("fixture primaryWindowRef = %q, want the empty ref the delete leaves",
			project.Spec.PrimaryWindowRef)
	}
	if err := store.registry.Validate(); err != nil {
		t.Fatalf("the zero-Window fixture is not a valid registry: %v", err)
	}
	for _, session := range tmux.sessions {
		if session.opts[tmuxopts.ProjectUIDSession] == projectUID {
			t.Fatalf("the zero-Window fixture still has a live session %q", session.name)
		}
	}
}

// windowsWithUID counts the live tmux windows mirroring one stable Window uid.
// One Window is one tmux window; two would mean the create both adopted the new
// session's initial window and then made a second one for the same identity.
func windowsWithUID(tmux *fakeTmux, uid string) []string {
	var ids []string
	for _, session := range tmux.sessions {
		for _, window := range session.windows {
			if window.opts[tmuxopts.WindowUID] == uid {
				ids = append(ids, session.name+"/"+window.id)
			}
		}
	}
	return ids
}

// A Project that is registered, stopped, and owns no Window is the state a
// migration or a recovery leaves behind. Creating the first Agent there used to
// fail the whole operation with
// `validate registry: project "beta" has 1 Windows but no primaryWindowRef`:
// the Window was allocated and then nothing named it as the primary. The create
// route now prepares the canonical topology and the runtime on the way.
func TestCreateAgentRevivesAStoppedZeroWindowProject(t *testing.T) {
	t.Parallel()

	store := newFakeResourceStore(t)
	tmux := newFakeTmux()
	stopAndEmptyProject(t, store, tmux, "prj-beta")
	command, launcher := newTestAgentCreateCommand(t, store, tmux)

	stdout, stderr, err := runRoute(t, command,
		"agent", "--provider", "codex", "--interactive-only", "--project", "uid:prj-beta",
		"--window", "revived", "--create-window", "--name", "revived-agent")
	if err != nil {
		t.Fatalf("create agent on a stopped zero-Window Project = stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "agent/revived-agent created") {
		t.Fatalf("stdout = %q, want the created Agent", stdout)
	}
	if len(launcher.plans) != 1 {
		t.Fatalf("provider launches = %d, want one", len(launcher.plans))
	}
	if err := store.registry.Validate(); err != nil {
		t.Fatalf("the written registry does not validate: %v", err)
	}

	window := windowNamed(t, store, "prj-beta", "revived")
	project, _ := store.registry.Project("prj-beta")
	if project.Spec.PrimaryWindowRef != window.Metadata.UID {
		t.Fatalf("primaryWindowRef = %q, want the Window the create made, %q",
			project.Spec.PrimaryWindowRef, window.Metadata.UID)
	}
	if project.Status.Session == nil || !project.Status.Session.Live {
		t.Fatalf("project session = %#v, want a live projection", project.Status.Session)
	}
	if live := windowsWithUID(tmux, window.Metadata.UID); len(live) != 1 {
		t.Fatalf("tmux windows mirroring window/%s = %v, want exactly one", window.Metadata.UID, live)
	}

	agents := store.registry.AgentsOf(window.Metadata.UID)
	if len(agents) != 1 || agents[0].Metadata.Name != "revived-agent" {
		t.Fatalf("agents of the revived Window = %#v, want one named revived-agent", agents)
	}
	pane, ok := store.registry.Pane(agents[0].Status.PaneRef)
	if !ok || pane.Metadata.OwnerUID() != agents[0].Metadata.UID {
		t.Fatalf("agent paneRef %q does not resolve back to an owned Pane", agents[0].Status.PaneRef)
	}
}

// The same revival through the Window route: `create window --provider` on a
// stopped zero-Window Project allocates its Window before the runtime exists,
// so it meets the same seam and must reach the same canonical shape.
func TestCreateWindowRevivesAStoppedZeroWindowProject(t *testing.T) {
	t.Parallel()

	store := newFakeResourceStore(t)
	tmux := newFakeTmux()
	stopAndEmptyProject(t, store, tmux, "prj-beta")
	command, _ := newTestAgentCreateCommand(t, store, tmux)

	stdout, stderr, err := runRoute(t, command,
		"window", "--project", "uid:prj-beta", "--name", "revived")
	if err != nil {
		t.Fatalf("create window on a stopped zero-Window Project = stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	if err := store.registry.Validate(); err != nil {
		t.Fatalf("the written registry does not validate: %v", err)
	}
	window := windowNamed(t, store, "prj-beta", "revived")
	project, _ := store.registry.Project("prj-beta")
	if project.Spec.PrimaryWindowRef != window.Metadata.UID {
		t.Fatalf("primaryWindowRef = %q, want %q", project.Spec.PrimaryWindowRef, window.Metadata.UID)
	}
	if live := windowsWithUID(tmux, window.Metadata.UID); len(live) != 1 {
		t.Fatalf("tmux windows mirroring window/%s = %v, want exactly one", window.Metadata.UID, live)
	}
}

// Regression guard for the Projects the change must not touch: a live Project
// keeps its primary Window when a later Window is created, and a stopped
// Project that still owns Windows keeps its own primary while its first Window
// is adopted into the new session and the created Window gets a window of its
// own.
func TestCreateAgentLeavesAnExistingPrimaryWindowRefAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		projectUID  string
		wantPrimacy string
		live        bool
	}{
		{name: "live project", projectUID: "prj-alpha", wantPrimacy: "win-alpha-main", live: true},
		{name: "stopped project that still owns Windows", projectUID: "prj-beta", wantPrimacy: "win-beta-main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newFakeResourceStore(t)
			tmux := newFakeTmux()
			if tt.live {
				seedExactCreateAgentTarget(t, tmux)
			}
			command, _ := newTestAgentCreateCommand(t, store, tmux)

			stdout, stderr, err := runRoute(t, command,
				"agent", "--provider", "codex", "--interactive-only", "--project", "uid:"+tt.projectUID,
				"--window", "added", "--create-window", "--name", "added-agent")
			if err != nil {
				t.Fatalf("create agent = stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
			project, _ := store.registry.Project(tt.projectUID)
			if project.Spec.PrimaryWindowRef != tt.wantPrimacy {
				t.Fatalf("primaryWindowRef = %q, want the unchanged %q",
					project.Spec.PrimaryWindowRef, tt.wantPrimacy)
			}
			added := windowNamed(t, store, tt.projectUID, "added")
			if live := windowsWithUID(tmux, added.Metadata.UID); len(live) != 1 {
				t.Fatalf("tmux windows mirroring the added window/%s = %v, want exactly one",
					added.Metadata.UID, live)
			}
			if live := windowsWithUID(tmux, tt.wantPrimacy); len(live) != 1 {
				t.Fatalf("tmux windows mirroring the primary window/%s = %v, want exactly one",
					tt.wantPrimacy, live)
			}
			if err := store.registry.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}
