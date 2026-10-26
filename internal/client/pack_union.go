package client

import (
	"encoding/json"
	"fmt"
)

// Pack-version unions, decoded by their discriminator.
//
// NamedPackTool.tool is a `oneOf` on `runtime` whose mapping is not one-to-one
// (`native` and `openclaw` both map to BuiltinToolRef). The normalizer's
// drop_ambiguous_discriminators pass therefore removes its discriminator, so
// oapi-codegen generates As*/From* helpers but no ValueByDiscriminator(). We
// read `runtime` from the raw JSON here and pick the variant ourselves.
//
// OrgPackVersion.connections[] keeps a one-to-one discriminator on `kind`, so
// a generated ValueByDiscriminator() exists. DecodePackConnection still reads
// `kind` itself, because ValueByDiscriminator() returns an untyped interface{}
// and an untyped error for an unknown value. This way callers get the same
// exactly-one-of shape and UnknownDiscriminatorError from both unions.
//
// An unknown discriminator value is an error, never a silent drop: a pack
// item the provider cannot represent must not disappear from a list that HCL
// feeds into an exclusive assignment resource.

// Discriminator values for NamedPackTool.tool (`runtime`).
const (
	PackToolRuntimeMcp      = "mcp"
	PackToolRuntimeNative   = "native"
	PackToolRuntimeOpenclaw = "openclaw"
)

// Discriminator values for OrgPackVersion.connections[] (`kind`).
const (
	PackConnectionKindMcpCatalogEntry = "mcp_catalog_entry"
	PackConnectionKindVaultSecret     = "vault_secret"
)

// UnknownDiscriminatorError reports a union member whose discriminator this
// client version does not know, typically because the API gained a variant
// after the provider was built.
type UnknownDiscriminatorError struct {
	Union         string // e.g. "NamedPackTool.tool"
	Field         string // the discriminator property, e.g. "runtime"
	Value         string // the value received ("" when absent)
	KnownValues   []string
	PresentInJSON bool
}

func (e *UnknownDiscriminatorError) Error() string {
	if !e.PresentInJSON {
		return fmt.Sprintf("%s has no %q discriminator (expected one of %q)", e.Union, e.Field, e.KnownValues)
	}
	return fmt.Sprintf("%s has unknown %s %q (this provider knows %q)", e.Union, e.Field, e.Value, e.KnownValues)
}

// PackTool is a decoded NamedPackTool.tool. Exactly one of Mcp / Builtin is
// non-nil; Runtime is the discriminator value that selected it.
type PackTool struct {
	Runtime string
	Mcp     *McpToolRef
	Builtin *BuiltinToolRef
}

// DecodePackTool dispatches a NamedPackTool.tool on its `runtime` field.
func DecodePackTool(t NamedPackTool_Tool) (PackTool, error) {
	raw, err := t.MarshalJSON()
	if err != nil {
		return PackTool{}, fmt.Errorf("reading NamedPackTool.tool: %w", err)
	}
	var disc struct {
		Runtime *string `json:"runtime"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return PackTool{}, fmt.Errorf("decoding NamedPackTool.tool runtime: %w", err)
	}
	known := []string{PackToolRuntimeMcp, PackToolRuntimeNative, PackToolRuntimeOpenclaw}
	if disc.Runtime == nil {
		return PackTool{}, &UnknownDiscriminatorError{Union: "NamedPackTool.tool", Field: "runtime", KnownValues: known}
	}
	switch rt := *disc.Runtime; rt {
	case PackToolRuntimeMcp:
		v, err := t.AsMcpToolRef()
		if err != nil {
			return PackTool{}, fmt.Errorf("decoding mcp tool: %w", err)
		}
		return PackTool{Runtime: rt, Mcp: &v}, nil
	case PackToolRuntimeNative, PackToolRuntimeOpenclaw:
		v, err := t.AsBuiltinToolRef()
		if err != nil {
			return PackTool{}, fmt.Errorf("decoding %s tool: %w", rt, err)
		}
		return PackTool{Runtime: rt, Builtin: &v}, nil
	default:
		return PackTool{}, &UnknownDiscriminatorError{
			Union: "NamedPackTool.tool", Field: "runtime", Value: rt, KnownValues: known, PresentInJSON: true,
		}
	}
}

// PackConnection is a decoded OrgPackVersion.connections[] item. Exactly one
// of Mcp / Vault is non-nil; Kind is the discriminator value that selected it.
type PackConnection struct {
	Kind  string
	Mcp   *NamedPackMcpConnection
	Vault *NamedPackVaultConnection
}

// DecodePackConnection dispatches a pack connection on its `kind` field.
func DecodePackConnection(c OrgPackVersion_Connections_Item) (PackConnection, error) {
	known := []string{PackConnectionKindMcpCatalogEntry, PackConnectionKindVaultSecret}
	raw, err := c.MarshalJSON()
	if err != nil {
		return PackConnection{}, fmt.Errorf("reading pack connection: %w", err)
	}
	var disc struct {
		Kind *string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return PackConnection{}, fmt.Errorf("decoding pack connection kind: %w", err)
	}
	if disc.Kind == nil {
		return PackConnection{}, &UnknownDiscriminatorError{Union: "OrgPackVersion.connections", Field: "kind", KnownValues: known}
	}
	switch kind := *disc.Kind; kind {
	case PackConnectionKindMcpCatalogEntry:
		v, err := c.AsNamedPackMcpConnection()
		if err != nil {
			return PackConnection{}, fmt.Errorf("decoding mcp_catalog_entry connection: %w", err)
		}
		return PackConnection{Kind: kind, Mcp: &v}, nil
	case PackConnectionKindVaultSecret:
		v, err := c.AsNamedPackVaultConnection()
		if err != nil {
			return PackConnection{}, fmt.Errorf("decoding vault_secret connection: %w", err)
		}
		return PackConnection{Kind: kind, Vault: &v}, nil
	default:
		return PackConnection{}, &UnknownDiscriminatorError{
			Union: "OrgPackVersion.connections", Field: "kind", Value: kind, KnownValues: known, PresentInJSON: true,
		}
	}
}
