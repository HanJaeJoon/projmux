package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crevissepartners/projmux/internal/integrations/agents/codexbroker"
)

func startInstallDrainHost(t *testing.T, domain, generation string, pid int, replaced bool) (*codexbroker.Host, codexbroker.Discovery) {
	t.Helper()
	key, err := codexbroker.NewEndpointKey("install-test-domain", generation)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := codexbroker.NewDiscovery(domain, key)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := codexbroker.NewBroker(codexbroker.Config{Endpoint: key, Opener: func(context.Context) (codexbroker.Endpoint, error) {
		return nil, errors.New("install must not open an upstream endpoint")
	}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := codexbroker.StartHost(codexbroker.HostConfig{Discovery: discovery, Broker: broker, IdleTimeout: -1, ImageReplaced: func() bool { return replaced }})
	if err != nil {
		_ = broker.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	// Several in-process test hosts share this test process's PID. Give their
	// selection hints distinct fixture values; credential/runtime authority
	// stays exactly as StartHost published it and Dial still verifies it.
	body, err := os.ReadFile(discovery.RecordPath())
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	record["pid"], err = json.Marshal(pid)
	if err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(discovery.RecordPath(), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return host, discovery
}

func TestInstallReplacementDrainsPublishedGenerationTargets(t *testing.T) {
	t.Parallel()
	domain := newBrokerStateDomain(t)
	first, firstDiscovery := startInstallDrainHost(t, domain, "generation-one", 101, true)
	second, _ := startInstallDrainHost(t, domain, "generation-two", 102, true)
	unrelated, _ := startInstallDrainHost(t, domain, "different-executable", 103, true)
	fallback, err := codexbroker.NewDiscovery(domain, codexbroker.DefaultEndpointKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(fallback.RecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("default endpoint must be unpublished")
	}
	result := requestInstallReplacementDrain(t.Context(), domain, []installReplacementTarget{{pid: 101}, {pid: 102}})
	if result.accepted != 2 || result.refusal != "drain-required" || result.failureStage != "" {
		t.Fatalf("drain result = accepted %d refusal %s stage %s", result.accepted, result.refusal, result.failureStage)
	}
	for _, host := range []*codexbroker.Host{first, second} {
		select {
		case <-host.Done():
		case <-time.After(time.Second):
			t.Fatal("accepted runtime did not drain")
		}
	}
	// A successor takes the first path before settle. Completion must still
	// describe the old socket, and must not wait for or drain the successor.
	successor, _ := startInstallDrainHost(t, domain, "generation-one", 104, false)
	if _, err := os.Lstat(firstDiscovery.SocketPath()); err != nil {
		t.Fatal(err)
	}
	command := installReplacementCommand{settle: time.Second, poll: time.Millisecond}
	if got := command.settleDrained(result); got != 2 {
		t.Fatalf("drained = %d, want two original runtimes", got)
	}
	for _, host := range []*codexbroker.Host{unrelated, successor} {
		if host.Stats().Draining {
			t.Fatal("the pass drained a process outside its residual targets")
		}
		select {
		case <-host.Done():
			t.Fatal("unrelated or successor runtime stopped")
		default:
		}
	}
}

func TestInstallReplacementWelcomeCannotStandInForResidualTarget(t *testing.T) {
	t.Parallel()
	domain := newBrokerStateDomain(t)
	host, _ := startInstallDrainHost(t, domain, "current-generation", 101, false)
	result := requestInstallReplacementDrain(t.Context(), domain, []installReplacementTarget{{pid: 101}})
	if result.accepted != 0 || result.failureStage != "handshake" || result.refusal != "" {
		t.Fatalf("current welcome = accepted %d refusal %s stage %s", result.accepted, result.refusal, result.failureStage)
	}
	if host.Stats().Draining {
		t.Fatal("current image was drained")
	}
}

func TestInstallReplacementMissingPublishedTargetIsDiscoveryFailure(t *testing.T) {
	t.Parallel()
	domain := newBrokerStateDomain(t)
	result := requestInstallReplacementDrain(t.Context(), domain, []installReplacementTarget{{pid: 101}})
	if result.accepted != 0 || result.failureStage != "discovery" || result.refusal != "host-unavailable" {
		t.Fatalf("missing target = accepted %d refusal %s stage %s", result.accepted, result.refusal, result.failureStage)
	}
	if _, err := os.Lstat(filepath.Join(domain, "broker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing target lookup created discovery artifacts")
	}
}

func TestInstallReplacementFailureStagesArePersistedWithoutIdentity(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"discovery", "dial", "handshake"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			outcome := runTestInstallReplacement(t, installReplacementFixture{
				vintage: projmuxProcessVintage{Supported: true, Roles: []projmuxProcessRoleVintage{{Role: codexControlPlaneRoleBroker, Processes: 1, Replaced: 1}}},
				refusal: "host-unavailable", failureStage: stage, goneAfter: -1, stateDir: dir,
				targets: []installReplacementTarget{{role: codexControlPlaneRoleBroker, pid: 74129, revision: "private-revision"}},
			})
			if outcome.FailureStage != stage || outcome.Refusal != "host-unavailable" {
				t.Fatalf("outcome = %+v", outcome)
			}
			body, err := os.ReadFile(filepath.Join(dir, installReplacementFile))
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"74129", "private-revision", dir, "pid", "credential", "socket"} {
				if bytes.Contains(body, []byte(private)) {
					t.Fatalf("identity leaked into persisted diagnostics: %s", private)
				}
			}
		})
	}
}

func TestInstallReplacementReconcilesTargetsThatExitAfterCensus(t *testing.T) {
	t.Parallel()
	root := newBrokerStateDomain(t)
	var stderr bytes.Buffer
	command := installReplacementCommand{
		stateDir: func() (string, error) { return root, nil },
		readVintage: func(time.Time) projmuxProcessVintage {
			return projmuxProcessVintage{Supported: true, Roles: []projmuxProcessRoleVintage{{Role: codexControlPlaneRoleBroker, Processes: 1, Replaced: 1}}}
		},
		requestDrain: func(ctx context.Context) installReplacementDrainResult {
			// The fresh exact target snapshot is empty after the earlier role
			// census saw a broker. There is no target to request or await now.
			return requestInstallReplacementDrain(ctx, root, nil)
		},
	}
	if err := command.Run(&stderr); err != nil {
		t.Fatal(err)
	}
	outcome, ok := readInstallReplacementOutcome(filepath.Join(root, installReplacementFile))
	if !ok || outcome.Outcome != installReplacementOutcomeNoTarget || outcome.Attempted != 0 || outcome.Drained != 0 || outcome.FailureStage != "" || stderr.Len() != 0 {
		t.Fatalf("vanished target = %+v stderr=%q", outcome, stderr.String())
	}
}
