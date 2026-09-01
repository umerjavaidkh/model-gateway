// Command snapshotgen writes a demo snapshot so the gateway can be run and
// exercised without a control plane.
//
// It is a development tool, not part of the product: real snapshots are
// compiled by the control plane. It exists because "clone the repo and send it
// a request" should be two commands, and because it doubles as an executable
// example of what a snapshot actually contains.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

func main() {
	out := flag.String("out", "snapshot.pb", "file to write")
	pepper := flag.String("pepper", "", "the key pepper the gateway will run with")
	secret := flag.String("secret", "demo-secret", "the API key secret to provision")
	flag.Parse()

	if err := run(*out, *pepper, *secret); err != nil {
		fmt.Fprintln(os.Stderr, "snapshotgen:", err)
		os.Exit(1)
	}
}

func run(out, pepper, secret string) error {
	if pepper == "" {
		return errors.New("-pepper is required and must match GATEWAY_KEY_PEPPER")
	}
	now := time.Now().UTC()

	global := wire.EncodeGlobal(core.GlobalSpec{
		Version: core.LayerVersion{Number: 1},
		BuiltAt: now,
		Deployments: []core.Deployment{{
			ID:           "echo-1",
			Key:          core.RoutingKey{BaseModel: "echo-model"},
			Provider:     "echo",
			Endpoint:     "in-process",
			Region:       "local",
			TrustTier:    core.TrustInternal,
			Weight:       100,
			Cost:         core.Cost{InputPer1K: 0, OutputPer1K: 0},
			Capabilities: []core.Capability{core.CapabilityStreaming},
		}},
		Aliases: []core.ModelAlias{
			{Name: "fast", Targets: []core.RoutingKey{{BaseModel: "echo-model"}}},
		},
		TenantPrefixes:  map[core.KeyPrefix]core.TenantID{"demo": "demo"},
		PolicyBundleRef: "demo",
	})

	tenant := wire.EncodeTenant(core.TenantSpec{
		Tenant:  "demo",
		Version: core.LayerVersion{Number: 1},
		BuiltAt: now,
		Tier:    "demo",
		Budgets: []core.BudgetState{{
			ID: "demo-monthly", Scope: core.BudgetScopeOrg,
			LimitMicroUSD: 10_000_000, Hard: true, HeadroomBasisPoints: 500,
		}},
		Principals: []core.Principal{{
			KeyID: "demo-key", Tenant: "demo", Org: "demo-org",
			Models:       core.ModelAllowlist{AllowAll: true},
			Budgets:      []core.BudgetRef{{ID: "demo-monthly", Scope: core.BudgetScopeOrg}},
			DefaultClass: core.DataClassInternal,
		}},
		Keys: map[core.KeyLookup]core.KeyID{
			core.ComputeKeyLookup([]byte(pepper), secret): "demo-key",
		},
		MinTrustTier: core.TrustExternal,
	})

	if err := wire.SealGlobal(global); err != nil {
		return err
	}
	if err := wire.SealTenant(tenant); err != nil {
		return err
	}

	b, err := wire.Marshal(&pb.Snapshot{Global: global, Tenants: []*pb.TenantLayer{tenant}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		return err
	}

	fmt.Printf("wrote %s (%d bytes)\napi key: gw_demo_%s\nmodels:  echo-model, fast\n", out, len(b), secret)
	return nil
}
