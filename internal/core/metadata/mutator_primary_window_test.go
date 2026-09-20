package metadata

import "testing"

// A Project that lost its last Window keeps an empty spec.primaryWindowRef by
// design, so the next Window added under it has to reclaim the ref inside the
// same write. Without that, the very next AddWindow leaves a registry Validate
// rejects, which is the state a stopped Project's first
// `create agent --create-window` used to fail in.
func TestFirstProjectWindowClaimsThePrimaryWindowRefInTheSameWrite(t *testing.T) {
	t.Parallel()

	roots := dirSet{"/src/projmux": true}
	m := testMutator(roots)
	reg := NewRegistry()

	registered, err := registerFixture(m, &reg, "/src/projmux")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectUID := registered.Project.Metadata.UID
	if got := registered.Project.Spec.PrimaryWindowRef; got != registered.Windows[0].Metadata.UID {
		t.Fatalf("registration primaryWindowRef = %q, want %q", got, registered.Windows[0].Metadata.UID)
	}

	for _, window := range reg.WindowsOf(projectUID) {
		if err := m.DeleteWindow(&reg, window.Metadata.UID); err != nil {
			t.Fatalf("delete window %s: %v", window.Metadata.UID, err)
		}
	}
	stopped, ok := reg.Project(projectUID)
	if !ok {
		t.Fatal("project disappeared with its Windows")
	}
	if stopped.Spec.PrimaryWindowRef != "" {
		t.Fatalf("zero-Window primaryWindowRef = %q, want empty", stopped.Spec.PrimaryWindowRef)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("zero-Window Project must be a valid registry: %v", err)
	}

	window, panes, err := m.AddWindow(&reg, projectUID, BootstrapWindow{Name: "revived"}, "/bin/zsh", "op-revive")
	if err != nil {
		t.Fatalf("add window: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("registry after the first Window must validate: %v", err)
	}
	revived, _ := reg.Project(projectUID)
	if revived.Spec.PrimaryWindowRef != window.Metadata.UID {
		t.Fatalf("primaryWindowRef = %q, want the first Window %q",
			revived.Spec.PrimaryWindowRef, window.Metadata.UID)
	}
	if window.Spec.DefaultShellPaneRef != panes[0].Metadata.UID {
		t.Fatalf("defaultShellPaneRef = %q, want %q", window.Spec.DefaultShellPaneRef, panes[0].Metadata.UID)
	}
}

// Adoption repairs an empty ref and nothing else: a Project that already names
// a primary Window keeps naming it when later Windows are added, which is what
// keeps `--project P` target selection and the primary spellings unchanged.
func TestLaterProjectWindowsNeverRepointAnExistingPrimaryWindowRef(t *testing.T) {
	t.Parallel()

	roots := dirSet{"/src/projmux": true}
	m := testMutator(roots)
	reg := NewRegistry()

	registered, err := registerFixture(m, &reg, "/src/projmux")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectUID := registered.Project.Metadata.UID
	primary := registered.Project.Spec.PrimaryWindowRef

	for _, name := range []string{"second", "third"} {
		if _, _, err := m.AddWindow(&reg, projectUID, BootstrapWindow{Name: name}, "/bin/zsh", "op-"+name); err != nil {
			t.Fatalf("add window %s: %v", name, err)
		}
		stored, _ := reg.Project(projectUID)
		if stored.Spec.PrimaryWindowRef != primary {
			t.Fatalf("after %s primaryWindowRef = %q, want the unchanged %q",
				name, stored.Spec.PrimaryWindowRef, primary)
		}
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// A ControlSession is not a Project and owns no primaryWindowRef, so its
// Windows must not reach into the Project table at all.
func TestControlSessionWindowsLeaveEveryProjectPrimaryWindowRefAlone(t *testing.T) {
	t.Parallel()

	roots := dirSet{"/src/projmux": true}
	m := testMutator(roots)
	reg := NewRegistry()

	registered, err := registerFixture(m, &reg, "/src/projmux")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectUID := registered.Project.Metadata.UID
	for _, window := range reg.WindowsOf(projectUID) {
		if err := m.DeleteWindow(&reg, window.Metadata.UID); err != nil {
			t.Fatalf("delete window: %v", err)
		}
	}

	bound, err := m.BindControlSession(&reg, ControlSessionObservation{
		Session: "home",
		Windows: []ControlSessionWindow{{DisplayName: "zsh", Panes: []ControlSessionPane{{Command: "zsh"}}}},
	}, "/bin/zsh", "op-control", nil)
	if err != nil {
		t.Fatalf("bind control session: %v", err)
	}
	if _, _, err := m.AddWindowToManagedRoot(&reg, KindControlSession, bound.ControlSession.Metadata.UID,
		BootstrapWindow{Name: "control-window"}, "/bin/zsh", "/src/projmux", "op-control-window"); err != nil {
		t.Fatalf("add control session window: %v", err)
	}
	stored, _ := reg.Project(projectUID)
	if stored.Spec.PrimaryWindowRef != "" {
		t.Fatalf("project primaryWindowRef = %q, want empty", stored.Spec.PrimaryWindowRef)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
