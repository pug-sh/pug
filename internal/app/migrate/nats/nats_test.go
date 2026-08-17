package nats

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestCheckReplicaSupport(t *testing.T) {
	tests := []struct {
		name        string
		replicas    int
		clusterName string
		wantErr     bool
	}{
		{"default against a standalone server", 1, "", false},
		{"replicated against a cluster", 3, "nats", false},
		{"default against a cluster", 1, "nats", false},
		{"replicated against a standalone server", 3, "", true},
		{"blank env var against a cluster", 0, "nats", true},
		{"blank env var against a standalone server", 0, "", true},
		{"negative against a cluster", -1, "nats", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReplicaSupport(tt.replicas, tt.clusterName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkReplicaSupport(%d, %q) = %v, wantErr %v", tt.replicas, tt.clusterName, err, tt.wantErr)
			}
		})
	}
}

const probeStreamName = "migrate-replica-probe"

// TestReplicaGuardAgainstStandalone drives the real Run against the package's
// NATS container, which is standalone — the shape the guard exists for.
func TestReplicaGuardAgainstStandalone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	tn := testutil.SetupNATS(t)
	ctx := t.Context()

	dir := writeProbeSchema(t)
	t.Setenv("NATS_URL", tn.URL)
	t.Setenv("NATS_STREAMS_CONFIG", filepath.Join(dir, "streams.yaml"))
	t.Setenv("NATS_CONSUMERS_CONFIG", filepath.Join(dir, "consumers.yaml"))

	// NATS_STREAM_REPLICAS unset: the default has to keep reconciling against a
	// standalone server, which is what local dev, CI and every self-host are.
	if err := Run(ctx); err != nil {
		t.Fatalf("run at the default replica count: %v", err)
	}
	if got := probeReplicas(ctx, t, tn); got != 1 {
		t.Fatalf("probe stream replicas = %d after the default run, want 1", got)
	}

	t.Setenv("NATS_STREAM_REPLICAS", "3")

	err := Run(ctx)
	if err == nil {
		t.Fatal("expected Run to refuse replicas > 1 against a standalone server")
	}
	for _, want := range []string{"NATS_STREAM_REPLICAS", "3", "cluster"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// The assertion the guard actually buys: a standalone server accepts an R3
	// update of an existing R1 stream and only fails at its next restart, so
	// without the pre-flight the stream would read back as 3 here.
	if got := probeReplicas(ctx, t, tn); got != 1 {
		t.Fatalf("probe stream replicas = %d after the refused run, want 1", got)
	}
}

// writeProbeSchema writes a one-stream schema for Run to reconcile. It cannot use
// schema/nats/streams.yaml: SetupNATS has already created those streams on memory
// storage, and JetStream refuses to change a stream's storage type.
func writeProbeSchema(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	streams := `streams:
  - name: "` + probeStreamName + `"
    subjects: ["migratereplicaprobe.>"]
    description: "Replica guard probe"
    retention_policy: "limits"
    max_consumers: -1
    max_msgs: -1
    max_bytes: 1048576
    max_age: 1h
    storage: "memory"
`
	if err := os.WriteFile(filepath.Join(dir, "streams.yaml"), []byte(streams), 0o600); err != nil {
		t.Fatalf("write streams fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "consumers.yaml"), []byte("consumers: []\n"), 0o600); err != nil {
		t.Fatalf("write consumers fixture: %v", err)
	}

	return dir
}

func probeReplicas(ctx context.Context, t *testing.T, tn *testutil.TestNATS) int {
	t.Helper()

	nc, err := natsgo.Connect(tn.URL)
	if err != nil {
		t.Fatalf("connect to nats: %v", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("create jetstream: %v", err)
	}

	stream, err := js.Stream(ctx, probeStreamName)
	if err != nil {
		t.Fatalf("get stream %s: %v", probeStreamName, err)
	}

	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}

	return info.Config.Replicas
}
