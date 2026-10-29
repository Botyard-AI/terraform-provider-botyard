# Changelog

All notable changes to the Botyard Terraform provider are recorded here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release workflow publishes each version's section below as the body of its
GitHub release, and fails before tagging if the section is missing or empty. See
[RELEASING.md](RELEASING.md#changelog) for how entries move from `Unreleased` into
a version.

## [Unreleased]

## [0.7.0] - 2026-10-29

**This release has breaking changes.** See **Removed** for the upgrade steps: delete the removed keys from your configuration, then run one `terraform apply`.

### Removed

- **BREAKING — `botyard_bot`: `config.model` removed.** The block had no effect: the Botyard API no longer accepts `model` on OpenClaw config, and a bot's model chain comes from its LLM credential links. Set the model with `botyard_bot_credential_assignment`. (#39)
- **BREAKING — `botyard_bot`: `native_config.prompt_template` and `native_config.prompt_ref` removed.** The API removed both fields, and v0.5.1 and v0.6.0 already refused them at plan time. Native instructions live in bot files. (#39)

  **Upgrading:**
  1. Delete `config.model`, `native_config.prompt_template` and `native_config.prompt_ref` from your configuration.
  2. A leftover removed key is silently ignored, because Terraform drops undeclared object keys before the provider sees them. An expression that *reads* a removed attribute (for example an output) fails at validate time.
  3. `terraform show -json` fails with `unsupported attribute "prompt_ref"` (or `"model"`) for a bot whose state still holds a removed key. Run one `terraform apply` after upgrading; a no-change apply is enough. `plan` and `apply` are unaffected.

### Added

- **`botyard_bot`: `native_config.tool_images`** controls how images from tool results are passed to the model: `enabled`, `max_dimension_px` (1–8000) and `default_detail` (`auto` / `low` / `high` / `original`). (#39)
  - A declared block owns all three fields. A field you leave out is reset to the platform default, and `tool_images = {}` resets them all.
  - Removing the whole block leaves the current server values unchanged.
- **`botyard_bot`: `config.tool_search`** for OpenClaw bots, with the same `mode` (`always` / `auto` / `off`) and `auto_threshold_percent` as `native_config.tool_search`. Both are validated at plan time. (#39)
  - OpenClaw's tool search has no threshold, so on OpenClaw bots `auto` behaves like `always` and `auto_threshold_percent` is ignored.
- **`botyard_bot`: new `pack` block, to create a bot from a Bot Pack.** The bot is created with the pack installed in the same step, and removing the block later uninstalls the pack and keeps the bot. (#38)
  - Set `pack_version_id`, for example from `data.botyard_pack_version`. Optional `settings` and `connections` choose, per item, what the pack applies at creation. Each choice is a `choice` plus a payload for the variants that carry data, e.g. `use_server = { mcp_server_id = ... }`.
  - `create` connections that would fail mid-apply are refused at plan time: a vendor sign-in (OAuth), an unavailable setup, the wrong setup kind, or a metered connection without `accept_metered = true`.
  - The provider never replaces a bot, which would lose its conversations and memory. So adding `pack` to an existing bot, or changing `pack_version_id`, is a plan-time error until the API supports it. Changing `settings` or `connections` after creation is a warning; they only apply at creation.
  - Leaving `avatar_url` unset keeps the pack's avatar.
  - Import takes `<slug>`, `<slug>:<install_id>` or `<slug>:<pack_slug>`. `<slug>` alone adopts an install only when the bot has exactly one.
  - Bots without `pack` are unchanged and make no extra API calls.
- **`botyard_bot`: `suggestions_opt_out`** turns off skill **and** tool suggestions for a bot (one platform flag covers both). `true` opts out; an explicit `false` turns suggestions back on. Works the same for bots created with `pack`. (#43)
  - Leaving it out reads the current value and never changes it, so existing configurations plan clean after upgrading. Each refresh reads the flag with one extra API call.
  - An API key owns only the bots it created. For a bot created in the Botyard app or with another key, import, refresh and planning are supported, but updates and deletion require the API key that created the bot. The flag is also unreadable on such a bot: Terraform keeps the last value it knew (none after an import), and setting it fails with a "Not the bot's owner" error.
- **`botyard_bot_skill_assignment` and `botyard_bot_tool_assignment` warn before removing a pack's skills or tools.** These resources own the bot's complete set. On a bot with a pack installed, leaving the pack's items out unassigns them on apply, and the pack does not add them back. (#44)
  - The plan names each pack-installed skill or tool that a change, a destroy, or a `bot_slug` change (which replaces the resource and unassigns everything from the previous bot) would remove. It is a warning only; the plan and apply still go ahead.
  - Keep them with `setunion(data.botyard_pack_version.<name>.skill_ids, [...])`, or `tool_ids` for tools. The docs and examples have a "Bots with a pack" section.
  - The check reads the bot and its pack installs. It runs only when `skill_ids` / `tool_ids` or `bot_slug` changes, or the resource is destroyed, so an unchanged plan makes no extra calls. If the read fails, the plan says the check was skipped.
- **`botyard_bot`: runtime settings `auto_suspend_enabled`, `runtime_privilege_mode` (now writable) and a `resources` block.** Works the same for bots created with `pack`. (#46)
  - Each is sent only when the plan changes it: changing `runtime_privilege_mode` or `resources` restarts the bot, so an unrelated apply never resends them.
  - Leaving `auto_suspend_enabled` or `runtime_privilege_mode` out reads the current value and never changes it, so existing configurations plan clean after upgrading.
  - `resources` (`cpu_request`, `cpu_limit`, `memory_request`, `memory_limit`, `storage`) behaves like `botyard_mcp_server.resources`: an omitted block is never managed, a declared block applies only the quantities you set, and removing it resets the bot to the plan defaults. `storage` can only grow.
  - `auto_suspend_enabled = false` and `runtime_privilege_mode = "privileged"` need a plan that includes them; otherwise the API refuses the change with the plan's upgrade message.
- **New resource `botyard_bot_schedule`: a recurring schedule on a bot.** Set when it fires with exactly one of `cron`, `interval` or `rrule`, and what it does with exactly one of `prompt` (wake the bot) or `command` (run a command on its runtime, with exit-code classes, a recheck `window` and per-outcome routing). (#48)
  - Switching timing kind or action kind updates the schedule in place. Every update sends only the fields that changed.
  - **Terraform owns `enabled`.** A schedule paused in the app, or auto-paused after `max_consecutive_failures`, shows as drift, and the next apply resumes it.
  - Leaving `conversation_id` out adopts the conversation Botyard creates for the schedule, with no diff.
  - Command schedules need the `schedule:schedule_command.create` / `.update` permission on the API key's role. Without it, apply fails with an error naming the permission. Edits that leave the command unchanged still work without it.
  - The resource manages only the schedule it created or imported (`<bot_slug>/<schedule_id>`). Other schedules on the bot are left alone.
  - The next fire time is not stored in state, because it changes on every read.
- **New data source `botyard_bot_schedules`** lists every recurring schedule on a bot, including ones the bot created itself, with each one's `next_fire_at`. (#48)

### Changed

- **`botyard_bot`: updating or deleting a bot the API key does not own now fails with a "Not the bot's owner" error** that says why and what to do, instead of a generic HTTP 403. An API key owns only the bots it created. A 403 for a missing plan feature keeps the API's own message. (#46)

### Fixed

- **`botyard_mcp_server`: declaring a `resources` block on an existing server no longer fails** with "Provider produced inconsistent result after apply". Quantities you leave out now adopt the server's current values. On v0.6.x, work around it by declaring `resources` when the server is created. (#45)

## [0.6.1] - 2026-10-29

### Fixed

- **`botyard_bot`: adding a nested config block to a bot that already exists failed with "Provider produced inconsistent result after apply".** For example `config.compaction`, `config.heartbeat` or `native_config.tool_search`. Creating a bot with the block was not affected. (#40)
  - Fields you leave unset in the new block now show as `(known after apply)` and take the platform's defaults.
  - Unchanged blocks keep their prior values, so existing plans stay clean.
  - Workaround on v0.6.0: declare the block's fields explicitly, or create the bot with the block.

## [0.6.0] - 2026-10-26

### Added

- **`botyard_mcp_server`: new `resources` block** for `container_image` servers, to manage the pod's compute: `cpu_request`, `cpu_limit`, `memory_request`, `memory_limit` and `storage`. (#32)
  - Only the quantities you set are applied. The rest keep their current server value.
  - Changing unrelated attributes (for example `description`) sends no `resources` key, so **the pod is not restarted**.
  - Removing the whole block resets the server to the platform default. Leaving it out entirely means Terraform doesn't manage compute, and a size set in the Botyard app is left alone.
  - The API's normalised spellings (`2` → `2000m`, `2048Mi` → `2Gi`) are treated as equal, so plans stay clean.
  - Checked at plan time: the quantity format, request ≤ limit, and that the server is not `managed_remote`.
  - Plan maximums and billing limits are enforced by the API.
- **Bot catalogue packs: three new data sources.** (#35)
  - `botyard_packs` reads the pack catalogue, paginated, with an optional `slugs` filter.
  - `botyard_pack` reads one pack by slug, with a summary of its latest version.
  - `botyard_pack_version` reads one version's full contents: skills (with their pinned versions), tools, connections, settings and soul.
    - Look a version up by `pack_version_id`, or by `slug` for the latest.
    - It exposes `skill_ids` and `tool_ids` for use with `setunion` in `botyard_bot_skill_assignment` and `botyard_bot_tool_assignment`. Those resources are exclusive and would otherwise remove a pack's items from a bot.

  ```terraform
  data "botyard_pack_version" "crm" {
    slug = "crm-assistant"
  }

  resource "botyard_bot_skill_assignment" "example" {
    bot_slug  = botyard_bot.example.slug
    skill_ids = setunion(data.botyard_pack_version.crm.skill_ids, [data.botyard_skill.extra.id])
  }
  ```

### Notes

- **Slugs can be ambiguous.** A Botyard pack and one of your organization's own packs can share a slug. A slug lookup that matches more than one pack fails and lists the candidates. Use `botyard_packs` to find the one you want, then pin it with `pack_version_id`.
- An older pack version can only be pinned by `pack_version_id`. The API can't look one up by version number.
- Managing a bot's pack itself (create from a pack, uninstall) is coming in a later release.
- No breaking changes.

## [0.5.1] - 2026-10-26

### Fixed

- **`botyard_bot` with a `native_config` block failed on every create and update with a 422** (`NativeConfigPatch/prompt_ref: Extra inputs are not permitted`). The Botyard API removed the native `prompt_template` and `prompt_ref` fields. v0.5.0 still sent `prompt_ref`. (#33)
  - The provider no longer sends either field.
  - Existing v0.5.0 state that holds the old server value plans as a no-op, so you don't need to change your config unless you set these fields yourself.

### Deprecated

- `botyard_bot` `native_config.prompt_template` and `native_config.prompt_ref`. Setting either one is now a **plan-time error**. A native bot's instructions (its soul) live in its bot files. (#33)

### Changed

- **`botyard_bot` reads its config by `harness` instead of the retired `bot_type` key.** Reads of bots that declare a `config` or `native_config` block keep working after the Botyard API stops returning `bot_type`. This release works against API versions from both before and after that change. **Upgrade before the API change rolls out.** (#29)
- **Generated API client re-synced** to a current Botyard API spec. Provider behaviour is unchanged. (#34)
  - `botyard_mcp_server` (container image) create now sends `"resources": null` when no `resources` block is declared. The Botyard API has treated this the same as leaving the field out since v1.7.1990.
- Built with **Go 1.26**. **Building the provider from source now requires Go 1.26.** (#30)

### Security

- Go 1.26.10 and `golang.org/x/net` v0.60.0 resolve 10 Go standard-library and `x/net` advisories that have no Go 1.25 fix. (#30)

## [0.5.0] - 2026-10-08

### Added

- **`botyard_mcp_server_member` resource** manages one member (bot or user) of an MCP server and its role. It is non-authoritative: members added in the app, the creator's owner row and members derived from tool assignments are left alone. An existing membership is adopted on create. (#27)
- **`access` attribute on `botyard_mcp_server`** (`open` or `restricted`). When omitted, the provider adopts the server's current value. (#27)
- README: what Botyard is, links to the Botyard docs, and a quickstart. (#28)

## [0.4.0] - 2026-09-23

### Added

- **Native bots on `botyard_bot`.** New `hosting_type` and `harness` attributes (immutable) and a `native_config` block, so hosted native (`botyard_native`) bots can be declared on the existing resource. A block that does not match the bot's harness fails at plan time. `model` and `host_policy` are exposed read-only. (#24)

### Changed

- Generated API client re-synced to a current, native-aware Botyard API spec. (#23)

## [0.3.0] - 2026-09-07

### Added

- **Request headers on managed-remote `botyard_mcp_server`:** `static_headers`, `secret_headers` (Runtime Vault key paths, not secret values) and `acknowledged_credential_host`. A remote MCP server that needs a token can now be declared in Terraform. (#21)

### Changed

- Generated API client re-synced to a current Botyard API spec. (#21)

## [0.2.3] - 2026-09-01

### Added

- **`botyard_skill` resource** to manage an organization skill. (#20)

## [0.2.2] - 2026-08-04

### Fixed

- Bot template data sources preserve the template's raw config instead of reshaping it. (#19)

### Security

- Bumped `google.golang.org/grpc` for GO-2026-6061. (#19)

## [0.2.1] - 2026-08-04

### Added

- **Discovery data sources** over the organization's catalogues: `botyard_bot_template`, `botyard_bot_templates`, `botyard_credentials`, `botyard_mcp_servers`, `botyard_skill`, `botyard_skills`, `botyard_tool` and `botyard_tools`. (#18)

## [0.2.0] - 2026-07-22

### Added

- **`botyard_bot` resource** with create, read, update, delete and import. (#12)
- **`config` block on `botyard_bot`** for the OpenClaw bot configuration. (#14)
- **Assignment resources** that manage a bot's full set of skills, tools and credentials: `botyard_bot_skill_assignment` (#15), `botyard_bot_tool_assignment` (#16) and `botyard_bot_credential_assignment` (#17).
- `RELEASING.md` release runbook and Registry submission checklist. (#10)

## [0.1.2] - 2026-07-21

### Fixed

- Deleting a `botyard_mcp_server` or `botyard_vault_secret` no longer reports a false failure (the API's empty 204 response was parsed as JSON), which had broken destroy and replace. (#13)
- Editing a `botyard_mcp_server` no longer forces a replacement, and in-place updates no longer send `"slug": null`. Unconfigured computed attributes keep their prior value. (#13)

## [0.1.1] - 2026-07-21

### Removed

- The deprecated `tier` and the internal `cluster_id` attributes from the `botyard_bot` data source. (#11)

## [0.1.0] - 2026-07-21

### Added

- Initial release: provider configuration (`endpoint`, `api_key`, `org_id`, with `BOTYARD_*` environment fallbacks) and a generated Botyard API client. (#1, #2)
- **`botyard_bot` data source.** (#1)
- **`botyard_mcp_server` resource** for `container_image` and `managed_remote` servers. (#4)
- **`botyard_vault_secret` resource** with a write-only `secret_value` (never stored in state) and `bot_ids`. (#8)
- Registry documentation and examples. (#9)
- GPG-signed releases published to the Terraform Registry. (#3, #5)

### Security

- Bumped `google.golang.org/grpc` (GO-2026-4762) and `golang.org/x/net` (GO-2026-5026), and pinned a patched Go toolchain. (#6)

[Unreleased]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.2.3...v0.3.0
[0.2.3]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/Botyard-AI/terraform-provider-botyard/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/Botyard-AI/terraform-provider-botyard/releases/tag/v0.1.0
