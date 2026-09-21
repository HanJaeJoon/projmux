package codexbroker

import (
	"sync/atomic"
	"testing"
)

// TestADrainKeepsABoundBindingAndStillRefusesANewOne is C-2's Non-Guarantee
// standing next to its Guarantee in one runtime.
//
// The layer above now carries a write on a binding whose lifecycle read a
// drain refused, on the ground that the drain never took the binding. That
// only holds while what the drain blocks stays exactly what it blocked before:
// arrivals. This is the test that has to fail if the fallback above is ever
// read as a reason to admit new work down here too.
func TestADrainKeepsABoundBindingAndStillRefusesANewOne(t *testing.T) {
	discovery := newRuntimeDiscovery(t)
	var replaced atomic.Bool
	host, _, _ := startVintageHost(t, discovery, &replaced)
	live := dialTestClient(t, discovery, ProtocolRange{})
	binding, fence := boundRemote(t, live, "thread-one")

	// Positive control: before the publication this runtime serves both.
	if outcome, err := binding.Submit(t.Context(), fence, Mutation{Method: "turn/steer"}); err != nil ||
		outcome != MutationApplied {
		t.Fatalf("Submit() before the publication = %s, %v", outcome, err)
	}
	arrival, err := live.Bind(t.Context(), "thread-two", "", nil)
	if err != nil {
		t.Fatalf("Bind() before the publication = %v", err)
	}
	if err := arrival.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	// The install publishes a new image under this runtime, and the next
	// arriving client puts it into the drain.
	replaced.Store(true)
	if _, err := Dial(t.Context(), discovery, DialConfig{}); RefusalOf(err) != RefusalDrainRequired {
		t.Fatalf("Dial() after a publication = %v, want drain-required", err)
	}
	if !host.Stats().Draining {
		t.Fatal("a superseded runtime did not enter a drain")
	}

	// The bound thread keeps its control.
	if outcome, err := binding.Submit(t.Context(), fence, Mutation{Method: "turn/steer"}); err != nil ||
		outcome != MutationApplied {
		t.Fatalf("Submit() during a drain = %s, %v", outcome, err)
	}
	// Both ways of arriving at new work are still refused, and say so with the
	// same token the layer above reports.
	if _, err := live.Bind(t.Context(), "thread-three", "", nil); RefusalOf(err) != RefusalDrainRequired {
		t.Fatalf("Bind() during a drain = %v, want drain-required", err)
	}
	if _, err := Dial(t.Context(), discovery, DialConfig{}); RefusalOf(err) != RefusalDrainRequired {
		t.Fatalf("second Dial() during a drain = %v, want drain-required", err)
	}
}
