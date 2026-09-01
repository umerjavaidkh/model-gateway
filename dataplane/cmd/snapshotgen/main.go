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
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

func main() {
	out := flag.String("out", "snapshot.pb", "file to write")
	pepper := flag.String("pepper", "", "the key pepper the gateway will run with")
	secret := flag.String("secret", "demo-secret", "the API key secret to provision")
	endpoint := flag.String("endpoint", "", "an OpenAI-compatible base URL to register, e.g. https://api.openai.com/v1")
	model := flag.String("model", "gpt-4o-mini", "the model id at -endpoint")
	credential := flag.String("credential", "env:OPENAI_API_KEY", "credential reference for -endpoint")
	inputCost := flag.Int64("input-cost", 150, "micro-USD per 1k input tokens at -endpoint")
	outputCost := flag.Int64("output-cost", 600, "micro-USD per 1k output tokens at -endpoint")
	flag.Parse()

	if err := run(*out, *pepper, *secret, *endpoint, *model, *credential, *inputCost, *outputCost); err != nil {
		fmt.Fprintln(os.Stderr, "snapshotgen:", err)
		os.Exit(1)
	}
}

func run(out, pepper, secret, endpoint, model, credential string, inputCost, outputCost int64) error {
	if pepper == "" {
		return errors.New("-pepper is required and must match GATEWAY_KEY_PEPPER")
	}
	now := time.Now().UTC()

	deployments := []core.Deployment{{
		ID:           "echo-1",
		Key:          core.RoutingKey{BaseModel: "echo-model"},
		Provider:     "echo",
		Endpoint:     "in-process",
		Region:       "local",
		TrustTier:    core.TrustInternal,
		Weight:       100,
		Capabilities: []core.Capability{core.CapabilityStreaming},
	}}
	aliases := []core.ModelAlias{
		{Name: "fast", Targets: []core.RoutingKey{{BaseModel: "echo-model"}}},
	}

	// A real upstream is opt-in, so the default demo needs no account and no
	// network. Its trust tier is external, which is what a public API is.
	if endpoint != "" {
		deployments = append(deployments, core.Deployment{
			ID:            "upstream-1",
			Key:           core.RoutingKey{BaseModel: model},
			Provider:      "openai-compatible",
			Endpoint:      endpoint,
			Region:        "cloud",
			TrustTier:     core.TrustExternal,
			CredentialRef: credential,
			Weight:        100,
			// Priced, so the demo shows real cost attribution. A demo that
			// always reports zero teaches that the number does not matter.
			Cost:         core.Cost{InputPer1K: core.MicroUSD(inputCost), OutputPer1K: core.MicroUSD(outputCost)},
			Capabilities: []core.Capability{core.CapabilityStreaming},
		})
		aliases = append(aliases, core.ModelAlias{
			Name: "real", Targets: []core.RoutingKey{{BaseModel: model}},
		})
	}

	global := wire.EncodeGlobal(core.GlobalSpec{
		Version:         core.LayerVersion{Number: 1},
		BuiltAt:         now,
		Deployments:     deployments,
		Aliases:         aliases,
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

	fmt.Printf("wrote %s (%d bytes)\napi key: gw_demo_%s\nmodels:  %s\n",
		out, len(b), secret, strings.Join(modelNames(aliases, deployments), ", "))
	return nil
}

// modelNames lists what a caller may ask for: every alias, plus every concrete
// model id, since either is accepted.
func modelNames(aliases []core.ModelAlias, deployments []core.Deployment) []string {
	names := make([]string, 0, len(aliases)+len(deployments))
	for _, a := range aliases {
		names = append(names, a.Name)
	}
	for _, d := range deployments {
		names = append(names, d.Key.BaseModel)
	}
	return names
}
