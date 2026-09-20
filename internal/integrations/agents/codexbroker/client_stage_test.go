package codexbroker

import (
	"bufio"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestDialFailureStagesPreserveRefusalAndCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stage  DialStage
		listen bool
	}{
		{"missing discovery", DialStageDiscovery, false},
		{"stale socket", DialStageDial, false},
		{"peer closes before reply", DialStageHandshake, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newRuntimeDiscovery(t)
			if tc.stage != DialStageDiscovery {
				if err := prepareDiscoveryDir(d); err != nil {
					t.Fatal(err)
				}
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: d.SocketPath(), Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				t.Cleanup(func() { _ = listener.Close() })
				if err := writeRecord(d, discoveryRecord{Endpoint: d.Endpoint(), Runtime: "test-runtime", Credential: "test-credential"}); err != nil {
					t.Fatal(err)
				}
				if tc.listen {
					go func() {
						conn, err := listener.Accept()
						if err == nil {
							_, _ = readFrame(bufio.NewReader(conn))
							_ = conn.Close()
						}
					}()
				} else {
					_ = listener.Close()
				}
			}
			_, err := Dial(t.Context(), d, DialConfig{})
			if DialStageOf(err) != tc.stage || RefusalOf(err) != RefusalHostUnavailable {
				t.Fatalf("Dial() = %v stage=%s, want host-unavailable/%s", err, DialStageOf(err), tc.stage)
			}
			var refusal *BrokerError
			if !errors.As(err, &refusal) || refusal.Unwrap() == nil {
				t.Fatal("typed refusal or underlying cause lost")
			}
			if err.Error() != "codex broker refused: host-unavailable" {
				t.Fatalf("content-free error changed: %s", err)
			}
			if tc.stage == DialStageHandshake && !errors.Is(err, io.EOF) {
				t.Fatalf("handshake cause = %T, want EOF", refusal.Unwrap())
			}
		})
	}
}

func TestDrainReplyPrecedesIdleShutdown(t *testing.T) {
	t.Parallel()
	host, _ := startTestHost(t, newRuntimeDiscovery(t), -1, ProtocolRange{})
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	replied := make(chan struct{})
	go func() { host.refuseDrainSession(writer); close(replied) }()
	deadline := time.After(time.Second)
	for !host.Stats().Draining {
		select {
		case <-deadline:
			t.Fatal("drain did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Exercise both shutdown triggers while the refusal write is blocked.
	// Neither may sever that reply even though no work keeps this host alive.
	host.releaseBinding()
	host.idleFired()
	select {
	case <-host.Done():
		t.Fatal("host closed before its drain reply")
	case <-time.After(20 * time.Millisecond):
	}
	frame, err := readFrame(bufio.NewReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	if string(frame) != `{"kind":"refused","refusal":"drain-required"}` {
		t.Fatalf("drain reply = %s", frame)
	}
	select {
	case <-replied:
	case <-time.After(time.Second):
		t.Fatal("refusal write did not finish")
	}
	select {
	case <-host.Done():
	case <-time.After(time.Second):
		t.Fatal("idle drain did not close after its reply")
	}
}
