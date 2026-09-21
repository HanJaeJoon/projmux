package usagecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crevissepartners/projmux/internal/core/usage"
	"github.com/crevissepartners/projmux/internal/core/usage/runtimemodel"
	intrender "github.com/crevissepartners/projmux/internal/ui/render"
)

// status_runtime_model_test.go pins the runtime model element: where it sits
// in the Claude row, that it is the FIRST element the segment sheds, that the
// text tiers never spell it, and that a segment rendered without one is
// byte-identical to the pre-change output.

const runtimeModelFixture = "claude-opus-5"

func runtimeModelFixtureMap() map[string]string {
	return map[string]string{"claude": runtimeModelFixture}
}

func TestRuntimeModelRendersAfterLabelBeforeAge(t *testing.T) {
	t.Parallel()

	got := formatStatusUsageWithVisibility(statusGoldenSnapshots(3*time.Minute), 0, statusGoldenNow, hudVisibilityPreferences{}, runtimeModelFixtureMap())
	plain := intrender.StripTmuxEscapes(got)
	if !strings.HasPrefix(plain, "Claude "+runtimeModelFixture+" (3m) 5h [") {
		t.Fatalf("Claude row = %q, want the runtime model between the label and the age text", plain)
	}
	if strings.Count(plain, runtimeModelFixture) != 1 || strings.Contains(strings.SplitN(plain, statusModelSeparator, 2)[1], runtimeModelFixture) {
		t.Fatalf("runtime model leaked outside the Claude block: %q", plain)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "status-usage-runtime-model.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("runtime model golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRuntimeModelAbsentIsByteIdentical is the compatibility contract: a nil
// map, an empty map, and a map for a provider that is not on the row all render
// exactly what formatStatusUsage rendered before the element existed.
func TestRuntimeModelAbsentIsByteIdentical(t *testing.T) {
	t.Parallel()

	for _, fixture := range usageSweepFixtures() {
		for _, width := range append([]int{0}, usageSweepWidths...) {
			want := formatStatusUsage(fixture.snaps, width, statusGoldenNow)
			for name, models := range map[string]map[string]string{
				"nil":            nil,
				"empty":          {},
				"other-provider": {"antigravity": "gemini-3-pro"},
			} {
				if got := formatStatusUsageWithVisibility(fixture.snaps, width, statusGoldenNow, hudVisibilityPreferences{}, models); got != want {
					t.Fatalf("%s/%s/w%d drifted\n got %q\nwant %q", fixture.name, name, width, got, want)
				}
			}
		}
	}
}

// TestRuntimeModelIsShedBeforeEveryOtherElement asserts rule 1 as a width
// relation: below the widest width that has already lost the model name, the
// segment WITH a model is byte-identical to the segment WITHOUT one. Nothing
// else has been spent to make room for it.
func TestRuntimeModelIsShedBeforeEveryOtherElement(t *testing.T) {
	t.Parallel()

	for _, fixture := range usageSweepFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			const limit = 300
			lossWidth := -1
			for w := limit; w >= 1; w-- {
				out := intrender.StripTmuxEscapes(formatStatusUsageWithVisibility(fixture.snaps, w, statusGoldenNow, hudVisibilityPreferences{}, runtimeModelFixtureMap()))
				if !strings.Contains(out, runtimeModelFixture) {
					lossWidth = w
					break
				}
			}
			if lossWidth < 0 {
				t.Fatal("the runtime model is never shed; the fixture cannot prove the ordering")
			}
			// The unbounded render still has every other element, so the model
			// went while everything else was intact.
			full := intrender.StripTmuxEscapes(formatStatusUsage(fixture.snaps, 0, statusGoldenNow))
			atLoss := intrender.StripTmuxEscapes(formatStatusUsage(fixture.snaps, lossWidth, statusGoldenNow))
			if atLoss != full {
				t.Fatalf("at the width the model is shed (%d) another element is already gone:\n with %q\n full %q", lossWidth, atLoss, full)
			}
			for w := lossWidth; w >= 1; w-- {
				with := formatStatusUsageWithVisibility(fixture.snaps, w, statusGoldenNow, hudVisibilityPreferences{}, runtimeModelFixtureMap())
				without := formatStatusUsage(fixture.snaps, w, statusGoldenNow)
				if with != without {
					t.Fatalf("width %d: a shed runtime model still changes the render\n with    %q\n without %q", w, with, without)
				}
			}
		})
	}
}

// TestRuntimeModelNeverReachesTextTiers: the colorless tiers spell no model
// name, because rule 1 runs long before rule 5 drops the bars.
func TestRuntimeModelNeverReachesTextTiers(t *testing.T) {
	t.Parallel()

	for w := 1; w <= 120; w++ {
		out := formatStatusUsageWithVisibility(statusGoldenSnapshots(3*time.Minute), w, statusGoldenNow, hudVisibilityPreferences{}, runtimeModelFixtureMap())
		if !strings.Contains(out, "#[") && strings.Contains(out, runtimeModelFixture) {
			t.Fatalf("width %d text tier spells the runtime model: %q", w, out)
		}
	}
}

func TestHUDRuntimeModelsFromReadsTheSidecarUnderVisibility(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	on := hudVisibilityPreferences{runtimeModels: map[string]bool{"claude": true}}
	off := hudVisibilityPreferences{runtimeModels: map[string]bool{"claude": false}}

	if got := hudRuntimeModelsFrom(stateDir, on); len(got) != 0 {
		t.Fatalf("no sidecar yet, got %v", got)
	}
	if err := runtimemodel.Write(stateDir, runtimemodel.Record{Provider: "claude", Model: runtimeModelFixture, Source: "PostModelSwitch"}); err != nil {
		t.Fatal(err)
	}
	if got := hudRuntimeModelsFrom(stateDir, on); got["claude"] != runtimeModelFixture {
		t.Fatalf("visible sidecar = %v", got)
	}
	if got := hudRuntimeModelsFrom(stateDir, off); len(got) != 0 {
		t.Fatalf("Settings off still loaded %v", got)
	}
	if got := hudRuntimeModelsFrom(stateDir, hudVisibilityPreferences{}); len(got) != 0 {
		t.Fatalf("missing preference should read as hidden, got %v", got)
	}
	// Codex declares no runtime model capability: a stray sidecar is ignored.
	if err := runtimemodel.Write(stateDir, runtimemodel.Record{Provider: "codex", Model: "gpt-5", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := hudRuntimeModelsFrom(stateDir, hudVisibilityPreferences{runtimeModels: map[string]bool{"claude": true, "codex": true}}); len(got) != 1 {
		t.Fatalf("codex sidecar leaked into the HUD: %v", got)
	}
}

// TestHUDRuntimeModelTextIsBoundedAndEscaped: the identifier is provider
// payload data, so control characters, `#` (a tmux format opener) and long
// values are neutralised the way opaque bucket ids are.
func TestHUDRuntimeModelTextIsBoundedAndEscaped(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	on := hudVisibilityPreferences{runtimeModels: map[string]bool{"claude": true}}
	cases := []struct{ raw, want string }{
		{raw: "claude-opus-5", want: "claude-opus-5"},
		{raw: "claude-opus-5[1m]", want: "claude-opus-5[1m]"},
		{raw: "bad\nmodel#[fg=red]", want: `bad\nmodel\x23[fg=red]`},
		{raw: strings.Repeat("m", 40), want: strings.Repeat("m", 31) + "…"},
	}
	for _, tc := range cases {
		if err := runtimemodel.Write(stateDir, runtimemodel.Record{Provider: "claude", Model: tc.raw, Source: "test"}); err != nil {
			t.Fatal(err)
		}
		if got := hudRuntimeModelsFrom(stateDir, on)["claude"]; got != tc.want {
			t.Fatalf("display of %q = %q, want %q", tc.raw, got, tc.want)
		}
	}
	// The escaped text keeps the HUD free of injected tmux formats.
	out := formatStatusUsageWithVisibility(statusGoldenSnapshots(3*time.Minute), 0, statusGoldenNow, hudVisibilityPreferences{}, map[string]string{"claude": `bad\nmodel\x23[fg=red]`})
	if strings.Contains(out, "#[fg=red]") {
		t.Fatalf("runtime model injected a tmux format: %q", out)
	}
}

// TestHUDProviderCapabilitiesDeclareTheRuntimeModelRow pins which providers
// own a Settings `Model` toggle: Claude only, default on.
func TestHUDProviderCapabilitiesDeclareTheRuntimeModelRow(t *testing.T) {
	t.Parallel()

	for _, capability := range HUDProviderCapabilities() {
		switch capability.Model {
		case "claude":
			if capability.RuntimeModel == nil || capability.RuntimeModel.Key != "model" || capability.RuntimeModel.Label != "Model" || capability.RuntimeModel.DefaultVisibility != "on" {
				t.Fatalf("claude runtime model capability = %+v", capability.RuntimeModel)
			}
		default:
			if capability.RuntimeModel != nil {
				t.Fatalf("%s unexpectedly declares a runtime model row", capability.Model)
			}
		}
	}
}

// TestRuntimeModelSnapshotsUnaffected: the element is render-only. The HUD
// snapshot projection the popup and the web read has no model to carry.
func TestRuntimeModelSnapshotsUnaffected(t *testing.T) {
	t.Parallel()

	snaps := statusGoldenSnapshots(3 * time.Minute)
	prefs := hudVisibilityPreferences{
		providers:     map[string]bool{"claude": true, "codex": true},
		windows:       map[string]map[usage.Window]bool{"claude": {usage.Window5h: true, usage.WindowWeekly: true}, "codex": {usage.Window5h: true, usage.WindowWeekly: true}},
		runtimeModels: map[string]bool{"claude": true},
	}
	if got, want := len(hudSnapshotsUnder(snaps, prefs)), 3; got != want {
		t.Fatalf("hud snapshots = %d, want %d", got, want)
	}
}
