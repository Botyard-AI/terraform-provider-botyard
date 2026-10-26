package client

import (
	"encoding/json"
	"errors"
	"testing"
)

func tool(t *testing.T, raw string) NamedPackTool_Tool {
	t.Helper()
	var v NamedPackTool_Tool
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return v
}

func conn(t *testing.T, raw string) OrgPackVersion_Connections_Item {
	t.Helper()
	var v OrgPackVersion_Connections_Item
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return v
}

func TestDecodePackTool(t *testing.T) {
	got, err := DecodePackTool(tool(t, `{"runtime":"mcp","tool_id":"t1","slug":"s","name":"n","title":null,"mcp_server":"hubspot"}`))
	if err != nil {
		t.Fatal(err)
	}
	mcp, ok := got.(*PackToolMcp)
	if !ok || mcp.Ref.McpServer != "hubspot" || mcp.Ref.ToolId != "t1" || got.Runtime() != "mcp" {
		t.Fatalf("mcp: %#v", got)
	}
	// Both builtin runtimes land on PackToolBuiltin; the discriminator is
	// derived from the variant's own typed field.
	for _, rt := range []string{"native", "openclaw"} {
		got, err := DecodePackTool(tool(t, `{"runtime":"`+rt+`","tool_id":"t2","slug":"s","name":"n","title":"T"}`))
		if err != nil {
			t.Fatal(err)
		}
		b, ok := got.(*PackToolBuiltin)
		if !ok || got.Runtime() != rt || b.Ref.ToolId != "t2" {
			t.Fatalf("%s: %#v", rt, got)
		}
	}
}

func TestDecodePackTool_Unknown(t *testing.T) {
	_, err := DecodePackTool(tool(t, `{"runtime":"wasm","tool_id":"t"}`))
	var u *UnknownDiscriminatorError
	if !errors.As(err, &u) || u.Value != "wasm" || !u.PresentInJSON || u.Field != "runtime" {
		t.Fatalf("want UnknownDiscriminatorError for wasm, got %v", err)
	}
	_, err = DecodePackTool(tool(t, `{"tool_id":"t"}`))
	if !errors.As(err, &u) || u.PresentInJSON {
		t.Fatalf("want missing-discriminator error, got %v", err)
	}
	// A non-string discriminator is a decode error, not a panic.
	if _, err = DecodePackTool(tool(t, `{"runtime":7}`)); err == nil {
		t.Fatal("want error for numeric runtime")
	}
}

func TestDecodePackConnection(t *testing.T) {
	got, err := DecodePackConnection(conn(t, `{"kind":"mcp_catalog_entry","required":true,"purpose":null,"mcp_catalog_entry":{"mcp_catalog_entry_id":"ce","slug":"s","name":"n","logo_url":null,"logo_href":null},"tool_names":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := got.(*PackConnectionMcp); !ok || m.Conn.McpCatalogEntry.McpCatalogEntryId != "ce" || got.Kind() != "mcp_catalog_entry" {
		t.Fatalf("mcp: %#v", got)
	}
	got, err = DecodePackConnection(conn(t, `{"kind":"vault_secret","required":false,"purpose":"p","secret_spec":{"key_path":"a.b","display_name":"d","max_ttl_seconds":60}}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got.(*PackConnectionVault); !ok || v.Conn.SecretSpec.KeyPath != "a.b" || got.Kind() != "vault_secret" {
		t.Fatalf("vault: %#v", got)
	}
	_, err = DecodePackConnection(conn(t, `{"kind":"oauth_app"}`))
	var u *UnknownDiscriminatorError
	if !errors.As(err, &u) || u.Value != "oauth_app" {
		t.Fatalf("want UnknownDiscriminatorError, got %v", err)
	}
	_, err = DecodePackConnection(conn(t, `{"required":true}`))
	if !errors.As(err, &u) || u.PresentInJSON {
		t.Fatalf("want missing-discriminator error, got %v", err)
	}
}
