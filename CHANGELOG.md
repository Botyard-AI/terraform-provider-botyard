# Changelog

Notable changes to the Botyard Terraform provider. GoReleaser's generated
changelog is disabled (`.goreleaser.yml`), so this file is the source of release
notes: when cutting a release, retitle **Unreleased** to the new version and
paste the section into the GitHub release body (see `RELEASING.md`).

## Unreleased

This release contains a **breaking change** and must be a minor bump (`v0.5.0`).

### Breaking changes

- **`botyard_bot`: removed `config.model` (the `primary { provider, model }`
  block).** It never took effect against the Botyard API. The API no longer
  accepts `model` in a config patch, and the bot's model is derived from its LLM
  credential links, so the provider neither applied nor read back anything in this
  block.

  **Upgrade:** delete the whole `model = { ... }` block from `config`. The bot's
  behaviour does not change, because the block had no effect.

  Terraform will **not** stop you if you forget. `config` is a nested attribute
  written in object syntax (`config = { ... }`), and Terraform core converts that
  object to the schema's type by silently discarding keys the type does not
  declare. A leftover `model = { ... }` validates, plans and applies without error
  or warning; it is simply ignored, exactly as it was before this release.
  (Verified with Terraform v1.13.3.) Delete it anyway, so the configuration does
  not suggest a model setting that is not there.

  What **does** fail is any expression that reads the attribute, such as
  `botyard_bot.example.config.model.primary.model` in an output or another
  resource: `Unsupported attribute: This object does not have an attribute named
  "model"`. Remove those references. The value they read was never the bot's
  real model; a native bot exposes its derived model as
  `native_config.model_name`.

  ```hcl
  resource "botyard_bot" "example" {
    name = "Example"
    config = {
      thinking_default = "high"
      # model = { primary = { provider = "botyard", model = "gpt-5.4" } }  # delete this
    }
  }
  ```

  **Where the model is configured instead:** on the bot's LLM credential links,
  with `botyard_bot_credential_assignment`. Credentials with `scope = "llm"` form
  the model chain in `ordinal` order (0 is tried first, then the fallbacks), and
  each can set a `default_model`:

  ```hcl
  resource "botyard_bot_credential_assignment" "example" {
    bot_slug = botyard_bot.example.slug
    credentials = [
      { credential_id = "cred_anthropic_primary", scope = "llm", ordinal = 0, default_model = "claude-opus-4-8" },
      { credential_id = "cred_openai_fallback",   scope = "llm", ordinal = 1 },
    ]
  }
  ```

  **Existing state** needs no manual step. Stored state written by v0.4.0 and
  earlier that still contains `config.model` is read without error, and the
  attribute is dropped on the first plan or refresh. Once the block is deleted
  from configuration, the plan shows no changes to the bot.
