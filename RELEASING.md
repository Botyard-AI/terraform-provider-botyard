# Releasing & publishing to the Terraform Registry

This provider is published to the [public Terraform
Registry](https://registry.terraform.io/providers/Botyard-AI/botyard) as
`Botyard-AI/botyard`. Releases are **cut manually** so that several merges batch
into one deliberate, semver-tagged, GPG-signed release that the Registry ingests.

## Current status — bootstrap complete ✅

The one-time Registry onboarding is **done**, and **`v0.1.0` is live** on the
Registry (protocol `6.0`, signed `SHA256SUMS`, all four docs pages). Verified:

- namespace `Botyard-AI` claimed;
- the release signing key registered on the namespace (signature verifies against
  the Registry-served key);
- the provider connected, so the repo's `release` webhook auto-ingests new tags;
- `v0.1.0` published and ingested.

**So the ongoing, actionable path is [cutting subsequent (bump-based)
releases](#cutting-a-release) — you do not re-run the one-time onboarding, and you
do not re-cut `v0.1.0`.** The [onboarding section](#one-time-registry-onboarding-completed--reference--recovery)
below is retained for verification and disaster recovery only.

---

## Cutting a release

Releases are produced by the **Release** workflow
(`.github/workflows/release.yml`), triggered manually.

1. Ensure `main` is at the commit you want to release and CI is green.
2. GitHub → **Actions → Release → Run workflow**, on `main`:
   - Pick a **bump** — `patch` / `minor` / `major`. The next `vX.Y.Z` is derived
     from the latest tag. With `v0.1.0` current: `patch → v0.1.1`,
     `minor → v0.2.0`, `major → v1.0.0`.
   - Leave **version** blank. Only set an explicit version to force a specific
     one — and note that an **already-published version fails closed** by design
     (you cannot re-cut `v0.1.0`; see re-run safety).
3. The workflow:
   - derives/validates the version (strict `vMAJOR.MINOR.PATCH`, no leading
     zeros, no pre-release/build metadata),
   - creates and pushes the annotated tag,
   - imports the GPG key from the `public` Environment,
   - runs **GoReleaser**, which cross-compiles the binaries, packages them as the
     Registry-expected zips, GPG-signs `SHA256SUMS`, and publishes the GitHub
     release with `terraform-registry-manifest.json` attached.
4. The repo's `release` webhook notifies the Registry, which ingests the new
   version (see [Verify a release ingested](#verify-a-release-ingested)).

### Re-run safety

If a run fails after the tag was pushed, re-dispatch the **same commit**: the
workflow detects and reuses the tag on that commit instead of bumping past it,
accepts an existing tag only when it points at the dispatched commit, and refuses
to clobber an already-published GitHub release (the version-check step fails
closed unless the releases API confirms 404 — which is also why re-cutting an
already-published version like `v0.1.0` fails).

### First release (`v0.1.0`) — completed bootstrap evidence

`v0.1.0` was cut with an explicit **version = `v0.1.0`** and is published +
Registry-ingested. It is recorded here as evidence; **do not re-cut it** — use a
bump for the next release.

---

## One-time Registry onboarding (completed — reference & recovery)

These steps were done once and do not need to be repeated. They are documented
for verification and for rebuilding the setup if it is ever lost. A `Botyard-AI`
org owner / GitHub admin is required.

### 1. Register the public GPG signing key

The Registry verifies every release's `SHA256SUMS` signature against a GPG public
key registered on the publishing namespace, and Terraform re-verifies it at
`terraform init`.

- The **ASCII-armored public key** is added at **User Settings → Signing Keys**
  on the Registry (<https://registry.terraform.io/settings/gpg-keys>) — keys can
  be added for your personal namespace or any org where you are an admin
  (`Botyard-AI` here).
- The Registry accepts **RSA or DSA** keys (not the default ECC type); the
  provider's key is RSA-4096. Expected fingerprint:
  `CD5FEE19207E194DDF5AF99BC7E924C8D78CCDDC` (key id `C7E924C8D78CCDDC`) — verify
  the registered key matches.
- The **private** key and passphrase are held only in the Botyard-AI maintainers'
  internal secret escrow; retrieve the public key from there. No key material is
  committed to this repo.

### 2. Configure the release signing secrets

The Release workflow reads two secrets from the repo's **`public` GitHub
Environment** (Settings → Environments → `public`), set by maintainers from the
same escrow as the public key:

- `GPG_PRIVATE_KEY` — the ASCII-armored **private** signing key.
- `PASSPHRASE` — its passphrase.

This makes the signature the Registry verifies chain to the registered public key.

### 3. Connect the provider

- Sign in to <https://registry.terraform.io> with a GitHub account that admins
  `Botyard-AI`. (The account's granted scopes are visible under GitHub
  **Settings → Applications → Authorized OAuth Apps → "Terraform Registry
  Application"**.)
- Top-right **Publish → Provider**, then select the `Botyard-AI` org and the
  `terraform-provider-botyard` repository.
- Publishing installs a **webhook on the GitHub repo subscribed to `release`
  events**, so every future release notifies the Registry for ingestion.
- The repo already satisfies the Registry's structural requirements: the
  `terraform-provider-botyard` name, a valid `terraform-registry-manifest.json`
  (`protocol_versions: ["6.0"]`), and generated `docs/` in the standard layout.

---

## Verify a release ingested

After a release is published:

- Confirm the version appears at
  <https://registry.terraform.io/providers/Botyard-AI/botyard>.
- Smoke-test consumption:

  ```hcl
  terraform {
    required_providers {
      botyard = {
        source  = "Botyard-AI/botyard"
        version = "0.1.0" # or the version you released
      }
    }
  }
  ```

  `terraform init` should download and verify the signed provider.

### Troubleshooting

- **A new release does not appear on the Registry** → the repo's `release`
  webhook may have missed it (ingestion is timing-sensitive: the release, the
  webhook, and the asset uploads race). Recovery: in the GitHub repo, remove any
  existing `registry.terraform.io` webhooks, then click **Resync** on the
  provider's settings page in the Registry to recreate the webhook; check recent
  deliveries under repo **Settings → Webhooks**.
- **Signature verification failed on ingest** → the public key registered under
  the namespace's Signing Keys does not match the `GPG_PRIVATE_KEY` used to sign.
  Re-check onboarding steps 1–2 (same key pair).
- **Docs not showing on the provider page** → `docs/` must be committed in the
  standard layout (`index.md` + `resources/` + `data-sources/`); it is generated
  by `make docs` and enforced by CI's docs-drift check.
