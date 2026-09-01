package snapshot_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/snapshot"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

func writeSnapshot(t *testing.T, tamper func(*pb.Snapshot)) string {
	t.Helper()

	global := wire.EncodeGlobal(core.GlobalSpec{
		Version: core.LayerVersion{Number: 1},
		Deployments: []core.Deployment{
			{ID: "echo-1", Key: core.RoutingKey{BaseModel: "m"}, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
		},
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
	})
	tenant := wire.EncodeTenant(core.TenantSpec{Tenant: "acme", Version: core.LayerVersion{Number: 1}})
	if err := wire.SealGlobal(global); err != nil {
		t.Fatalf("SealGlobal: %v", err)
	}
	if err := wire.SealTenant(tenant); err != nil {
		t.Fatalf("SealTenant: %v", err)
	}

	msg := &pb.Snapshot{Global: global, Tenants: []*pb.TenantLayer{tenant}}
	if tamper != nil {
		tamper(msg)
	}
	b, err := wire.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	path := filepath.Join(t.TempDir(), "snapshot.pb")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadFile(t *testing.T) {
	snap, err := snapshot.LoadFile(writeSnapshot(t, nil))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := snap.GlobalVersion().Number; got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
}

func TestLoadFileRejectsATamperedLayer(t *testing.T) {
	// Verification happens before decoding, so corruption is reported as
	// corruption rather than as whatever validation error it happens to cause.
	path := writeSnapshot(t, func(msg *pb.Snapshot) {
		msg.GetGlobal().GetDeployments()[0].Weight = 50
	})
	if _, err := snapshot.LoadFile(path); err == nil {
		t.Fatal("expected a digest mismatch")
	}
}

func TestLoadFileErrors(t *testing.T) {
	if _, err := snapshot.LoadFile(filepath.Join(t.TempDir(), "missing.pb")); err == nil {
		t.Fatal("expected a missing file to error")
	}

	garbage := filepath.Join(t.TempDir(), "garbage.pb")
	if err := os.WriteFile(garbage, []byte{0xff, 0xff, 0xff}, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := snapshot.LoadFile(garbage); err == nil {
		t.Fatal("expected garbage to fail parsing")
	}
}
