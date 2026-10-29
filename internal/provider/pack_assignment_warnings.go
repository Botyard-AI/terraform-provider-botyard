package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// The exclusive assignment resources (botyard_bot_skill_assignment,
// botyard_bot_tool_assignment) converge a bot on exactly the declared set.
// On a bot with a pack installed, the pack's skills and tools are part of that
// live set, so leaving them out of the config silently strips them on apply.
// The helpers here find those items so ModifyPlan can warn at plan time.
//
// The check is advisory: it never fails a plan. If the bot does not exist yet,
// or a read fails, the warning is skipped (a failed read says so).

// packAssignmentKind selects which install items a resource cares about. It
// carries its own names, so neither kind is an implicit default.
type packAssignmentKind struct {
	nounText string // "skill" / "tool"
	attrName string // the resource's id-set attribute
}

var (
	packAssignmentSkills = packAssignmentKind{nounText: "skill", attrName: "skill_ids"}
	packAssignmentTools  = packAssignmentKind{nounText: "tool", attrName: "tool_ids"}
)

func (k packAssignmentKind) noun() string { return k.nounText }

func (k packAssignmentKind) attr() string { return k.attrName }

// packVersionRef is the pack_version expression yielding this kind's ids.
func (k packAssignmentKind) packVersionRef() string {
	return "data.botyard_pack_version.<name>." + k.attr()
}

// packItemRef is one live pack item: the catalog id the assignment resource
// sees, and the pack it came with.
type packItemRef struct {
	ID       string
	PackSlug string
}

// livePackItems returns the bot's live pack items of this kind, keyed by
// catalog id. Only items whose bot-side assignment still exists count: a
// recorded item whose skill or tool was already removed is not "live".
// ok is false when the bot cannot be checked (does not exist or a read
// failed); a failed read adds a warning.
func livePackItems(ctx context.Context, data *providerData, slug string, kind packAssignmentKind,
	diags *diag.Diagnostics,
) (map[string]packItemRef, bool) {
	skip := func(err error) (map[string]packItemRef, bool) {
		diags.AddAttributeWarning(path.Root(kind.attr()), "Could not check for pack-installed "+kind.noun()+"s",
			fmt.Sprintf("Reading the pack installs of bot %q failed, so this plan was not checked for "+
				"pack-installed %ss it would remove: %s", slug, kind.noun(), err))
		return nil, false
	}

	bot, err := data.client.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(ctx, data.orgID, slug)
	if err != nil {
		return skip(err)
	}
	if bot.StatusCode() == 404 {
		return nil, false // created later in this apply: nothing installed yet
	}
	if bot.JSON200 == nil {
		return skip(fmt.Errorf("reading the bot returned HTTP %d: %s", bot.StatusCode(), describeAPIError(bot.Body)))
	}
	botID := bot.JSON200.Id
	var uid openapi_types.UUID
	if err := uid.UnmarshalText([]byte(botID)); err != nil {
		return skip(fmt.Errorf("bot id %q is not a UUID: %w", botID, err))
	}

	resp, err := data.client.ListBotPackInstallsV1OrgsOrgIdPacksInstallsGetWithResponse(ctx, data.orgID,
		&client.ListBotPackInstallsV1OrgsOrgIdPacksInstallsGetParams{BotIds: []openapi_types.UUID{uid}})
	if err != nil {
		return skip(err)
	}
	if resp.JSON200 == nil {
		return skip(fmt.Errorf("reading installs returned HTTP %d: %s", resp.StatusCode(), describeAPIError(resp.Body)))
	}

	live := map[string]packItemRef{}
	for _, b := range resp.JSON200.Bots {
		if b.BotId != botID {
			continue
		}
		for _, in := range b.Installs {
			for _, raw := range in.Items {
				if ref, ok := livePackItem(ctx, raw, kind, in.PackSlug); ok {
					live[ref.ID] = ref
				}
			}
		}
	}
	return live, true
}

// livePackItem decodes one install item and returns it when it is a live item
// of this kind. Item kinds this provider does not know are skipped: they are
// not skills or tools, and a newer API must not break planning.
func livePackItem(ctx context.Context, raw client.BotPackInstallView_Items_Item, kind packAssignmentKind,
	packSlug string,
) (packItemRef, bool) {
	item, err := client.DecodePackInstallItem(raw)
	if err != nil {
		var unknown *client.UnknownDiscriminatorError
		if !errors.As(err, &unknown) {
			tflog.Warn(ctx, "skipping undecodable pack install item", map[string]any{"error": err.Error()})
		}
		return packItemRef{}, false
	}
	switch v := item.(type) {
	case *client.PackInstallSkill:
		if kind == packAssignmentSkills && v.Item.BotSkillId != nil && v.Item.SkillId != nil {
			return packItemRef{ID: *v.Item.SkillId, PackSlug: packSlug}, true
		}
	case *client.PackInstallTool:
		if kind == packAssignmentTools && v.Item.BotToolId != nil && v.Item.ToolCatalogId != nil {
			return packItemRef{ID: *v.Item.ToolCatalogId, PackSlug: packSlug}, true
		}
	case *client.PackInstallOtherItem:
	}
	return packItemRef{}, false
}

// strippedPackItems returns the live pack items that are not in keep, sorted
// by id for a stable message.
func strippedPackItems(live map[string]packItemRef, keep []string) []packItemRef {
	kept := make(map[string]bool, len(keep))
	for _, id := range keep {
		kept[id] = true
	}
	var out []packItemRef
	for id, ref := range live {
		if !kept[id] {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// removedPackItems returns the live pack items that are in remove (the ids a
// destroy unassigns), sorted by id.
func removedPackItems(live map[string]packItemRef, remove []string) []packItemRef {
	var out []packItemRef
	for _, id := range remove {
		if ref, ok := live[id]; ok {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func describePackItems(items []packItemRef) string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, fmt.Sprintf("  - %s (pack %q)", it.ID, it.PackSlug))
	}
	return strings.Join(lines, "\n")
}

// packPlanMode says which half of a plan warnPackAssignmentPlan checks.
type packPlanMode int

const (
	// packPlanUpdate: create or in-place update; plan is the new set on slug.
	packPlanUpdate packPlanMode = iota
	// packPlanDestroy: the resource is destroyed; state's ids are unassigned.
	packPlanDestroy
	// packPlanReplaceOld: bot_slug changes, which forces replacement. Terraform
	// plans that as one change and never calls ModifyPlan for its delete half,
	// so the old bot's unassignment (Delete of state's ids) is checked here.
	packPlanReplaceOld
)

// warnPackAssignmentPlan is the shared ModifyPlan body. state and plan are the
// prior and planned sets (null when absent). For packPlanDestroy and
// packPlanReplaceOld, slug and state are the prior bot's and plan is ignored.
func warnPackAssignmentPlan(ctx context.Context, data *providerData, kind packAssignmentKind,
	slug types.String, state, plan types.Set, mode packPlanMode, diags *diag.Diagnostics,
) {
	destroy := mode != packPlanUpdate
	if data == nil || slug.IsUnknown() || slug.IsNull() {
		return
	}
	if !destroy && (plan.IsUnknown() || plan.Equal(state)) {
		return // unknown until apply, or no change: nothing new is stripped
	}
	set := plan
	if destroy {
		set = state
	}
	ids, known := knownStrings(set)
	if !known || (destroy && len(ids) == 0) {
		return // ids still unknown (e.g. from resources not yet created), or nothing to remove
	}

	live, ok := livePackItems(ctx, data, slug.ValueString(), kind, diags)
	if !ok || len(live) == 0 {
		return
	}

	noun := kind.noun()
	if mode == packPlanReplaceOld {
		if removed := removedPackItems(live, ids); len(removed) > 0 {
			diags.AddAttributeWarning(path.Root("bot_slug"), "Replacing this assignment removes pack-installed "+noun+"s",
				fmt.Sprintf("Changing bot_slug replaces this resource: every %s in its %s is unassigned from the "+
					"previous bot %q, including these that a pack installed there:\n%s\n\nThat bot loses them. To "+
					"keep them, manage it with its own assignment that includes the pack's %ss.", noun, kind.attr(),
					slug.ValueString(), describePackItems(removed), noun))
		}
		return
	}
	if destroy {
		if removed := removedPackItems(live, ids); len(removed) > 0 {
			diags.AddWarning("Destroying this assignment removes pack-installed "+noun+"s",
				fmt.Sprintf("Destroying this assignment unassigns every %s in its %s, including these that a pack "+
					"installed on bot %q:\n%s\n\nUnless the bot is destroyed as well, it loses them. Remove the "+
					"pack's %ss from %s first, or keep the resource.", noun, kind.attr(), slug.ValueString(),
					describePackItems(removed), noun, kind.attr()))
		}
		return
	}
	if stripped := strippedPackItems(live, ids); len(stripped) > 0 {
		diags.AddAttributeWarning(path.Root(kind.attr()), "Plan removes pack-installed "+noun+"s",
			fmt.Sprintf("This resource owns the bot's complete %s set, and %s leaves out these %ss that a pack "+
				"installed on bot %q:\n%s\n\nApplying unassigns them. To keep them, add the pack version's %ss "+
				"to %s, e.g.\n  setunion(%s, [...])", noun, kind.attr(), noun, slug.ValueString(),
				describePackItems(stripped), noun, kind.attr(), kind.packVersionRef()))
	}
}

// knownStrings returns a set's string elements, or false when the set or any
// element is null or unknown. Unlike setToStrSlice it never adds diagnostics:
// the check it feeds is advisory.
func knownStrings(s types.Set) ([]string, bool) {
	if s.IsNull() || s.IsUnknown() {
		return nil, false
	}
	out := make([]string, 0, len(s.Elements()))
	for _, e := range s.Elements() {
		v, ok := e.(types.String)
		if !ok || v.IsNull() || v.IsUnknown() {
			return nil, false
		}
		out = append(out, v.ValueString())
	}
	return out, true
}
