package wire

import (
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/protobuf/proto"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

// DigestPrefix names the hash used, so the format can change later without
// every stored digest becoming ambiguous.
const DigestPrefix = "sha256:"

// marshalOptions pins deterministic output.
//
// Protobuf is not canonical by default: map entries serialize in arbitrary
// order, so the same layer can produce different bytes on each call. A digest
// computed over non-deterministic bytes would differ between the control plane
// and the worker verifying it, and the layer would be rejected at random.
// Deterministic mode is stable within a protobuf release, which is the
// guarantee this relies on.
var marshalOptions = proto.MarshalOptions{Deterministic: true}

// Marshal serializes a wire message deterministically.
func Marshal(msg proto.Message) ([]byte, error) {
	b, err := marshalOptions.Marshal(msg)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "serializing a snapshot message")
	}
	return b, nil
}

// UnmarshalSnapshot parses a full snapshot.
func UnmarshalSnapshot(b []byte) (*pb.Snapshot, error) {
	var msg pb.Snapshot
	if err := proto.Unmarshal(b, &msg); err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "parsing a snapshot")
	}
	return &msg, nil
}

// UnmarshalGlobal parses a standalone global layer.
func UnmarshalGlobal(b []byte) (*pb.GlobalLayer, error) {
	var msg pb.GlobalLayer
	if err := proto.Unmarshal(b, &msg); err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "parsing a global layer")
	}
	return &msg, nil
}

// UnmarshalTenant parses a standalone tenant layer. Tenant layers travel on
// their own: that is the point of the layering, since a budget edit should not
// reship the catalog.
func UnmarshalTenant(b []byte) (*pb.TenantLayer, error) {
	var msg pb.TenantLayer
	if err := proto.Unmarshal(b, &msg); err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "parsing a tenant layer")
	}
	return &msg, nil
}

// A layer's digest content-addresses it, which is what lets a worker skip a
// re-fetch it already holds and lets two workers claiming the same version be
// proven to hold the same bytes.
//
// The digest is computed over the layer with its own digest field cleared,
// since a field cannot cover itself. Callers on the producing side Seal a
// layer; callers on the consuming side Verify it.

// DigestGlobal computes the content address of a global layer.
func DigestGlobal(msg *pb.GlobalLayer) (string, error) {
	clone, ok := proto.Clone(msg).(*pb.GlobalLayer)
	if !ok {
		return "", core.New(core.CodeInternal, "cloning a global layer produced the wrong type")
	}
	clearDigest(clone.GetVersion())
	return digestOf(clone)
}

// DigestTenant computes the content address of a tenant layer.
func DigestTenant(msg *pb.TenantLayer) (string, error) {
	clone, ok := proto.Clone(msg).(*pb.TenantLayer)
	if !ok {
		return "", core.New(core.CodeInternal, "cloning a tenant layer produced the wrong type")
	}
	clearDigest(clone.GetVersion())
	return digestOf(clone)
}

// SealGlobal stamps a global layer with its own digest.
func SealGlobal(msg *pb.GlobalLayer) error {
	d, err := DigestGlobal(msg)
	if err != nil {
		return err
	}
	ensureVersion(msg).Digest = d
	return nil
}

// SealTenant stamps a tenant layer with its own digest.
func SealTenant(msg *pb.TenantLayer) error {
	d, err := DigestTenant(msg)
	if err != nil {
		return err
	}
	ensureTenantVersion(msg).Digest = d
	return nil
}

// VerifyGlobal checks a global layer against its stamped digest.
//
// An unstamped layer passes. Digests are a corruption and mix-up check, not an
// authenticity one — anyone who can rewrite the layer can rewrite the digest.
// Authenticity is the transport's job: mTLS to the control plane, and signed
// manifests for registry components.
func VerifyGlobal(msg *pb.GlobalLayer) error {
	want := msg.GetVersion().GetDigest()
	if want == "" {
		return nil
	}
	got, err := DigestGlobal(msg)
	if err != nil {
		return err
	}
	if got != want {
		return core.Newf(core.CodeInvalidRequest,
			"global layer digest mismatch: computed %s, layer claims %s", got, want)
	}
	return nil
}

// VerifyTenant checks a tenant layer against its stamped digest.
func VerifyTenant(msg *pb.TenantLayer) error {
	want := msg.GetVersion().GetDigest()
	if want == "" {
		return nil
	}
	got, err := DigestTenant(msg)
	if err != nil {
		return err
	}
	if got != want {
		return core.Newf(core.CodeInvalidRequest,
			"tenant %q layer digest mismatch: computed %s, layer claims %s", msg.GetTenant(), got, want)
	}
	return nil
}

func digestOf(msg proto.Message) (string, error) {
	b, err := Marshal(msg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

func clearDigest(v *pb.LayerVersion) {
	if v != nil {
		v.Digest = ""
	}
}

func ensureVersion(msg *pb.GlobalLayer) *pb.LayerVersion {
	if msg.GetVersion() == nil {
		msg.Version = &pb.LayerVersion{}
	}
	return msg.GetVersion()
}

func ensureTenantVersion(msg *pb.TenantLayer) *pb.LayerVersion {
	if msg.GetVersion() == nil {
		msg.Version = &pb.LayerVersion{}
	}
	return msg.GetVersion()
}
