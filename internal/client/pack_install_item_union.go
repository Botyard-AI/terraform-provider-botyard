package client

import (
	"encoding/json"
	"fmt"
)

// BotPackInstallView.items[] is a `oneOf` on `kind` whose mapping is not
// one-to-one (`avatar`, `model` and `soul` all map to
// BotPackSingletonProvenance), so the normalizer drops its discriminator and
// no Discriminator() is generated. DecodePackInstallItem reads `kind` itself,
// like DecodePackTool. An unknown kind is an UnknownDiscriminatorError, never
// a silent drop; callers decide whether that is fatal.

// Discriminator values for BotPackInstallView.items[] (`kind`).
const (
	PackInstallItemKindSkill      = "skill"
	PackInstallItemKindTool       = "tool"
	PackInstallItemKindMcpServer  = "mcp_server"
	PackInstallItemKindSecretLink = "secret_link"
	PackInstallItemKindSetting    = "setting"
	PackInstallItemKindAvatar     = "avatar"
	PackInstallItemKindModel      = "model"
	PackInstallItemKindSoul       = "soul"
)

// PackInstallItem is a decoded install item: a sealed set of variants.
// Dispatch with a type switch on *PackInstallSkill / *PackInstallTool /
// *PackInstallOtherItem.
type PackInstallItem interface {
	// Kind is the `kind` discriminator value of this item.
	Kind() string
	isPackInstallItem()
}

// PackInstallSkill is a skill the install recorded.
type PackInstallSkill struct {
	Item BotPackSkillProvenance
}

// PackInstallTool is a tool the install recorded.
type PackInstallTool struct {
	Item BotPackToolProvenance
}

// PackInstallOtherItem is any other known item kind (MCP server, secret
// link, setting, avatar, model, soul). Its payload is not decoded: no caller
// in this provider reads it yet.
type PackInstallOtherItem struct {
	ItemKind string
}

func (*PackInstallSkill) Kind() string       { return PackInstallItemKindSkill }
func (*PackInstallTool) Kind() string        { return PackInstallItemKindTool }
func (o *PackInstallOtherItem) Kind() string { return o.ItemKind }

func (*PackInstallSkill) isPackInstallItem()     {}
func (*PackInstallTool) isPackInstallItem()      {}
func (*PackInstallOtherItem) isPackInstallItem() {}

// DecodePackInstallItem dispatches an install item on its `kind` field.
func DecodePackInstallItem(it BotPackInstallView_Items_Item) (PackInstallItem, error) {
	known := []string{
		PackInstallItemKindSkill, PackInstallItemKindTool, PackInstallItemKindMcpServer,
		PackInstallItemKindSecretLink, PackInstallItemKindSetting, PackInstallItemKindAvatar,
		PackInstallItemKindModel, PackInstallItemKindSoul,
	}
	raw, err := it.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("reading pack install item: %w", err)
	}
	var disc struct {
		Kind *string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return nil, fmt.Errorf("decoding pack install item kind: %w", err)
	}
	if disc.Kind == nil {
		return nil, &UnknownDiscriminatorError{Union: "BotPackInstallView.items", Field: "kind", KnownValues: known}
	}
	switch kind := *disc.Kind; kind {
	case PackInstallItemKindSkill:
		v, err := it.AsBotPackSkillProvenance()
		if err != nil {
			return nil, fmt.Errorf("decoding skill item: %w", err)
		}
		return &PackInstallSkill{Item: v}, nil
	case PackInstallItemKindTool:
		v, err := it.AsBotPackToolProvenance()
		if err != nil {
			return nil, fmt.Errorf("decoding tool item: %w", err)
		}
		return &PackInstallTool{Item: v}, nil
	case PackInstallItemKindMcpServer, PackInstallItemKindSecretLink, PackInstallItemKindSetting,
		PackInstallItemKindAvatar, PackInstallItemKindModel, PackInstallItemKindSoul:
		return &PackInstallOtherItem{ItemKind: kind}, nil
	default:
		return nil, &UnknownDiscriminatorError{
			Union: "BotPackInstallView.items", Field: "kind", Value: kind, KnownValues: known, PresentInJSON: true,
		}
	}
}
