package client

import (
	"encoding/json"
	"errors"
	"testing"
)

func installItem(t *testing.T, raw string) BotPackInstallView_Items_Item {
	t.Helper()
	var v BotPackInstallView_Items_Item
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return v
}

func TestDecodePackInstallItem(t *testing.T) {
	got, err := DecodePackInstallItem(installItem(t, `{"kind":"skill","bot_pack_item_id":"i1","origin":"added",`+
		`"created_at":"2026-10-01T00:00:00Z","bot_skill_id":"bs1","skill_id":"sk1"}`))
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := got.(*PackInstallSkill)
	if !ok || got.Kind() != "skill" || sk.Item.SkillId == nil || *sk.Item.SkillId != "sk1" ||
		sk.Item.BotSkillId == nil || *sk.Item.BotSkillId != "bs1" || sk.Item.Origin != BotPackItemOriginAdded {
		t.Fatalf("skill: %#v", got)
	}

	got, err = DecodePackInstallItem(installItem(t, `{"kind":"tool","bot_pack_item_id":"i2","origin":"adopted",`+
		`"created_at":"2026-10-01T00:00:00Z","bot_tool_id":null,"tool_catalog_id":"tc1"}`))
	if err != nil {
		t.Fatal(err)
	}
	tl, ok := got.(*PackInstallTool)
	if !ok || got.Kind() != "tool" || tl.Item.BotToolId != nil || tl.Item.ToolCatalogId == nil ||
		*tl.Item.ToolCatalogId != "tc1" || tl.Item.Origin != BotPackItemOriginAdopted {
		t.Fatalf("tool: %#v", got)
	}

	for _, k := range []string{"mcp_server", "secret_link", "setting", "avatar", "model", "soul"} {
		got, err := DecodePackInstallItem(installItem(t, `{"kind":"`+k+`","bot_pack_item_id":"i3"}`))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if o, ok := got.(*PackInstallOtherItem); !ok || o.Kind() != k {
			t.Fatalf("%s: %#v", k, got)
		}
	}
}

func TestDecodePackInstallItem_Unknown(t *testing.T) {
	_, err := DecodePackInstallItem(installItem(t, `{"kind":"hologram"}`))
	var u *UnknownDiscriminatorError
	if !errors.As(err, &u) || u.Value != "hologram" || !u.PresentInJSON || u.Field != "kind" {
		t.Fatalf("want UnknownDiscriminatorError for hologram, got %v", err)
	}
	_, err = DecodePackInstallItem(installItem(t, `{"bot_pack_item_id":"i"}`))
	if !errors.As(err, &u) || u.PresentInJSON {
		t.Fatalf("want missing-discriminator error, got %v", err)
	}
	if _, err = DecodePackInstallItem(installItem(t, `{"kind":7}`)); err == nil {
		t.Fatal("want error for numeric kind")
	}
	// A known kind with a malformed payload is a decode error, not a variant.
	if _, err = DecodePackInstallItem(installItem(t, `{"kind":"skill","skill_id":7}`)); err == nil {
		t.Fatal("want error for malformed skill payload")
	}
}
