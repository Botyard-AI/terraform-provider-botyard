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
	mcp, err := DecodePackTool(tool(t, `{"runtime":"mcp","tool_id":"t1","slug":"s","name":"n","title":null,"mcp_server":"hubspot"}`))
	if err != nil || mcp.Mcp == nil || mcp.Builtin != nil || mcp.Mcp.McpServer != "hubspot" || mcp.Runtime != "mcp" {
		t.Fatalf("mcp: %+v err=%v", mcp, err)
	}
	// Both builtin runtimes land on BuiltinToolRef, keeping their own runtime.
	for _, rt := range []string{"native", "openclaw"} {
		b, err := DecodePackTool(tool(t, `{"runtime":"`+rt+`","tool_id":"t2","slug":"s","name":"n","title":"T"}`))
		if err != nil || b.Builtin == nil || b.Mcp != nil || b.Runtime != rt || string(b.Builtin.Runtime) != rt {
			t.Fatalf("%s: %+v err=%v", rt, b, err)
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
	m, err := DecodePackConnection(conn(t, `{"kind":"mcp_catalog_entry","required":true,"purpose":null,"mcp_catalog_entry":{"mcp_catalog_entry_id":"ce","slug":"s","name":"n","logo_url":null,"logo_href":null},"tool_names":null}`))
	if err != nil || m.Mcp == nil || m.Vault != nil || m.Mcp.McpCatalogEntry.McpCatalogEntryId != "ce" {
		t.Fatalf("mcp: %+v err=%v", m, err)
	}
	v, err := DecodePackConnection(conn(t, `{"kind":"vault_secret","required":false,"purpose":"p","secret_spec":{"key_path":"a.b","display_name":"d","max_ttl_seconds":60}}`))
	if err != nil || v.Vault == nil || v.Mcp != nil || v.Vault.SecretSpec.KeyPath != "a.b" {
		t.Fatalf("vault: %+v err=%v", v, err)
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
