package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// sha256Hex is the reference hash used to assert secret_value_hash.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func strSet(ids ...string) types.Set {
	elems := make([]attr.Value, 0, len(ids))
	for _, id := range ids {
		elems = append(elems, types.StringValue(id))
	}
	return types.SetValueMust(types.StringType, elems)
}

// vaultModel builds a minimal valid resource model.
func vaultModel() VaultSecretResourceModel {
	return VaultSecretResourceModel{
		KeyPath:         types.StringValue("github.token"),
		DisplayName:     types.StringValue("GitHub token"),
		Description:     types.StringNull(),
		Sensitivity:     types.StringValue("secret"),
		AllowAllBots:    types.BoolValue(false),
		MaxTTLSeconds:   types.Int64Value(300),
		SecretValue:     types.StringValue("s3cr3t"),
		SecretValueHash: types.StringNull(),
		BotIDs:          types.SetNull(types.StringType),
	}
}

func TestValidateVaultSecretConfig(t *testing.T) {
	if d := validateVaultSecretConfig(vaultModel()); d.HasError() {
		t.Errorf("valid config errored: %v", d)
	}

	// bot_ids + allow_all_bots is a conflict.
	bad := vaultModel()
	bad.AllowAllBots = types.BoolValue(true)
	bad.BotIDs = strSet("11111111-1111-1111-1111-111111111111")
	if !validateVaultSecretConfig(bad).HasError() {
		t.Error("bot_ids with allow_all_bots=true should error")
	}

	// allow_all_bots=true with no bot_ids is fine.
	ok := vaultModel()
	ok.AllowAllBots = types.BoolValue(true)
	if validateVaultSecretConfig(ok).HasError() {
		t.Error("allow_all_bots=true without bot_ids should be valid")
	}

	// invalid sensitivity.
	bad = vaultModel()
	bad.Sensitivity = types.StringValue("public")
	if !validateVaultSecretConfig(bad).HasError() {
		t.Error("invalid sensitivity should error")
	}

	// out-of-range TTL.
	for _, v := range []int64{59, 3601} {
		bad = vaultModel()
		bad.MaxTTLSeconds = types.Int64Value(v)
		if !validateVaultSecretConfig(bad).HasError() {
			t.Errorf("max_ttl_seconds=%d should error", v)
		}
	}
	// boundary values are valid.
	for _, v := range []int64{60, 3600} {
		ok = vaultModel()
		ok.MaxTTLSeconds = types.Int64Value(v)
		if validateVaultSecretConfig(ok).HasError() {
			t.Errorf("max_ttl_seconds=%d should be valid", v)
		}
	}
}

func TestBuildCreateBody(t *testing.T) {
	m := vaultModel()
	m.BotIDs = strSet("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	body, diags := buildCreateBody(context.Background(), m, "s3cr3t")
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	obj := decodeObj(t, body)
	if jsonStr(t, obj["key_path"]) != "github.token" {
		t.Errorf("key_path = %s", obj["key_path"])
	}
	if jsonStr(t, obj["display_name"]) != "GitHub token" {
		t.Errorf("display_name = %s", obj["display_name"])
	}
	if jsonStr(t, obj["value"]) != "s3cr3t" {
		t.Errorf("create body must carry the plaintext value for the API; got %s", obj["value"])
	}
	if jsonStr(t, obj["sensitivity"]) != "secret" {
		t.Errorf("sensitivity = %s", obj["sensitivity"])
	}
	if string(obj["allow_all_bots"]) != "false" {
		t.Errorf("allow_all_bots = %s", obj["allow_all_bots"])
	}
	if string(obj["max_ttl_seconds"]) != "300" {
		t.Errorf("max_ttl_seconds = %s", obj["max_ttl_seconds"])
	}
	var ids []string
	if err := json.Unmarshal(obj["bot_ids"], &ids); err != nil || len(ids) != 2 {
		t.Errorf("bot_ids = %s (err %v)", obj["bot_ids"], err)
	}
}

func TestBuildUpdateBody_ValueOnlyWhenChanged(t *testing.T) {
	m := vaultModel()

	// unchanged secret: no value key (avoid needless re-encryption/rotation).
	body, diags := buildUpdateBody(m, "s3cr3t", false)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	obj := decodeObj(t, body)
	if _, ok := obj["value"]; ok {
		t.Error("update body must omit value when the secret is unchanged")
	}
	for _, k := range []string{"display_name", "description", "sensitivity", "allow_all_bots", "max_ttl_seconds"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("update body must always carry %q", k)
		}
	}
	// description is null when unset (reconciles to the desired absent value).
	if string(obj["description"]) != "null" {
		t.Errorf("description = %s, want null", obj["description"])
	}

	// changed secret: value present.
	body, diags = buildUpdateBody(m, "rotated", true)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	obj = decodeObj(t, body)
	if jsonStr(t, obj["value"]) != "rotated" {
		t.Errorf("update body must carry the new value when changed; got %s", obj["value"])
	}
}

func TestHashSecretValue(t *testing.T) {
	if got := hashSecretValue(types.StringValue("s3cr3t")); got.ValueString() != sha256Hex("s3cr3t") {
		t.Errorf("hash = %s, want %s", got.ValueString(), sha256Hex("s3cr3t"))
	}
	if !hashSecretValue(types.StringNull()).IsNull() {
		t.Error("null value should hash to null")
	}
	if !hashSecretValue(types.StringUnknown()).IsUnknown() {
		t.Error("unknown value should hash to unknown")
	}
	// distinct values produce distinct hashes; the hash never equals the plaintext.
	a := hashSecretValue(types.StringValue("a"))
	b := hashSecretValue(types.StringValue("b"))
	if a.ValueString() != sha256Hex("a") {
		t.Errorf("hash(a) = %s, want %s", a.ValueString(), sha256Hex("a"))
	}
	if a.Equal(b) {
		t.Error("distinct secrets must hash differently")
	}
	if a.ValueString() == "a" {
		t.Error("hash must not equal the plaintext")
	}
}

func TestMapPolicy_DoesNotTouchWriteOnlyOrLinks(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	resp := &client.SecretPolicyResponse{
		PolicyId:       "p-1",
		KeyPath:        "github.token",
		DisplayName:    "GitHub token",
		Description:    strp("read-only token"),
		Sensitivity:    client.RuntimeVaultSensitivitySecret,
		AllowAllBots:   false,
		LinkedBotCount: 2,
		MaxTtlSeconds:  600,
		CreatedAt:      ts,
		UpdatedAt:      ts,
	}
	// Sentinels prove mapPolicy leaves the write-only value, its hash, and the
	// bot-links set untouched (those are owned elsewhere).
	m := VaultSecretResourceModel{
		SecretValue:     types.StringValue("SENTINEL-VALUE"),
		SecretValueHash: types.StringValue("SENTINEL-HASH"),
		BotIDs:          strSet("keep-me"),
	}
	mapPolicy(resp, &m)

	if m.ID.ValueString() != "p-1" || m.KeyPath.ValueString() != "github.token" {
		t.Errorf("id/key_path = %q/%q", m.ID.ValueString(), m.KeyPath.ValueString())
	}
	if m.Description.ValueString() != "read-only token" || m.MaxTTLSeconds.ValueInt64() != 600 {
		t.Errorf("description/ttl = %q/%d", m.Description.ValueString(), m.MaxTTLSeconds.ValueInt64())
	}
	if m.LinkedBotCount.ValueInt64() != 2 {
		t.Errorf("linked_bot_count = %d", m.LinkedBotCount.ValueInt64())
	}
	if m.SecretValue.ValueString() != "SENTINEL-VALUE" {
		t.Error("mapPolicy must not write secret_value")
	}
	if m.SecretValueHash.ValueString() != "SENTINEL-HASH" {
		t.Error("mapPolicy must not overwrite secret_value_hash")
	}
	if elems := m.BotIDs.Elements(); len(elems) != 1 {
		t.Error("mapPolicy must not overwrite bot_ids")
	}
}

// TestSecretValueNeverInState is the core security assertion: after building
// the state model exactly as Create/Read/Update do, the write-only secret_value
// is null while its one-way hash is present — the plaintext never reaches state.
func TestSecretValueNeverInState(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	resp := &client.SecretPolicyResponse{
		PolicyId:      "p-1",
		KeyPath:       "github.token",
		DisplayName:   "GitHub token",
		Sensitivity:   client.RuntimeVaultSensitivitySecret,
		MaxTtlSeconds: 300,
		CreatedAt:     ts,
		UpdatedAt:     ts,
	}
	// As in Create: the plan model has the write-only value nullified.
	m := VaultSecretResourceModel{SecretValue: types.StringNull()}
	mapPolicy(resp, &m)
	m.SecretValueHash = hashSecretValue(types.StringValue("s3cr3t"))

	if !m.SecretValue.IsNull() {
		t.Fatal("secret_value must be null in the persisted state model")
	}
	if m.SecretValueHash.ValueString() != sha256Hex("s3cr3t") {
		t.Errorf("secret_value_hash = %s, want %s", m.SecretValueHash.ValueString(), sha256Hex("s3cr3t"))
	}
	if m.SecretValueHash.ValueString() == "s3cr3t" {
		t.Fatal("state must never contain the plaintext secret")
	}
}

func TestBotIDsEqual(t *testing.T) {
	var diags diag.Diagnostics
	ctx := context.Background()

	if !botIDsEqual(ctx, strSet("a", "b"), strSet("b", "a"), &diags) {
		t.Error("order should not matter")
	}
	if botIDsEqual(ctx, strSet("a"), strSet("a", "b"), &diags) {
		t.Error("different lengths should be unequal")
	}
	if botIDsEqual(ctx, strSet("a"), strSet("b"), &diags) {
		t.Error("different members should be unequal")
	}
	if !botIDsEqual(ctx, types.SetNull(types.StringType), types.SetNull(types.StringType), &diags) {
		t.Error("two null sets should be equal")
	}
	// an unknown planned set is treated as equal (no spurious bot-links write).
	if !botIDsEqual(ctx, types.SetUnknown(types.StringType), strSet("a"), &diags) {
		t.Error("unknown planned set should be treated as equal")
	}
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
}
