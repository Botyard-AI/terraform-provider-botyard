# Releasing & publishing to the Terraform Registry

This provider is distributed through the [public Terraform
Registry](https://registry.terraform.io/providers/Botyard-AI/botyard) as
`Botyard-AI/botyard`. Releases are **cut manually** so that several merges batch
into one deliberate, semver-tagged, GPG-signed release that the Registry ingests.

There are two phases:

1. **One-time Registry onboarding** — connect the provider and register its
   signing key (done once for the namespace). Most of this is human-gated.
2. **Cutting a release** — run the Release workflow; repeatable for every
   version.

---

## Phase 1 — One-time Registry onboarding

Do these once (a Botyard-AI org owner / GitHub admin is required).

### 1. Confirm the Registry namespace

- The `Botyard-AI` namespace is claimed on
  [registry.terraform.io](https://registry.terraform.io). The provider's source
  address is `Botyard-AI/botyard` (repo `Botyard-AI/terraform-provider-botyard`,
  provider type `botyard`).

### 2. Register the public GPG signing key on the namespace

The Registry verifies every release's `SHA256SUMS` signature against a GPG public
key registered on the publishing namespace. Without this, ingestion fails.

- [ ] Upload the provider's **ASCII-armored public GPG key** to the `Botyard-AI`
      namespace: registry.terraform.io → **Settings → GPG keys** for the
      organization, and add the key.
- [ ] Confirm the uploaded key matches the release signing key configured for
      this repo. Expected fingerprint:
      `CD5FEE19207E194DDF5AF99BC7E924C8D78CCDDC` (key id `C7E924C8D78CCDDC`).
      The public key, private key, and passphrase are held by the Botyard-AI
      maintainers in the internal secret escrow — retrieve the public key from
      there; do not commit any key material to this repo.

> The signing **private key** and **passphrase** never live in this repo. They
> are consumed only by the Release workflow (see Phase 2).

### 3. Configure the release signing secrets

The Release workflow reads two secrets from the repo's **`public` GitHub
Environment** (Settings → Environments → `public`):

- [ ] `GPG_PRIVATE_KEY` — the ASCII-armored **private** signing key.
- [ ] `PASSPHRASE` — its passphrase.

These are set by maintainers out-of-band from the same escrow as the public key
above, so the signature the Registry verifies chains to the registered key.

### 4. Connect the provider in the Registry UI

- [ ] Sign in to registry.terraform.io with a GitHub account that admins
      `Botyard-AI`.
- [ ] **Publish → Provider**, then select the
      `Botyard-AI/terraform-provider-botyard` repository. The Terraform Registry
      GitHub App must have access to the repo (grant it if the repo is not
      listed).
- [ ] The repo already satisfies the Registry's structural requirements: the
      `terraform-provider-botyard` name, a valid `terraform-registry-manifest.json`
      (`protocol_versions: ["6.0"]`), and generated `docs/` in the standard
      layout.

Once connected, the Registry ingests any existing and future signed releases for
the namespace.

---

## Phase 2 — Cutting a release

Releases are produced by the **Release** workflow
(`.github/workflows/release.yml`), triggered manually.

### Cut `v0.1.0` (first release)

1. Ensure `main` is at the commit you want to release and CI is green.
2. GitHub → **Actions → Release → Run workflow**, on `main`:
   - Set **version** to `v0.1.0` (an explicit version overrides the bump level;
     it is the safest choice for the first release).
   - Leave **bump** at its default (ignored when an explicit version is set).
3. Run it. The workflow:
   - derives/validates the version (strict `vMAJOR.MINOR.PATCH`, no leading
     zeros, no pre-release/build metadata),
   - creates and pushes the annotated tag,
   - imports the GPG key from the `public` Environment,
   - runs **GoReleaser**, which cross-compiles the binaries, packages them as the
     Registry-expected zips, GPG-signs `SHA256SUMS`, and publishes the GitHub
     release with `terraform-registry-manifest.json` attached.

### Subsequent releases

Run the same workflow and pick a **bump** (`patch` / `minor` / `major`); the next
`vX.Y.Z` is derived from the latest tag. Use the **version** field only to force a
specific version.

### Re-run safety

If a run fails after the tag was pushed, re-dispatch the **same commit**: the
workflow detects and reuses the tag on that commit instead of bumping past it,
accepts an existing tag only when it points at the dispatched commit, and refuses
to clobber an already-published GitHub release.

---

## Phase 3 — Verify ingestion

After the release is published:

- [ ] The Registry ingests the signed release automatically (once the provider is
      connected and the signing key is registered).
- [ ] Confirm the version appears at
      <https://registry.terraform.io/providers/Botyard-AI/botyard>.
- [ ] Smoke-test consumption:

  ```hcl
  terraform {
    required_providers {
      botyard = {
        source  = "Botyard-AI/botyard"
        version = "0.1.0"
      }
    }
  }
  ```

  `terraform init` should download and verify the signed provider.

### Troubleshooting

- **Signature verification failed on ingest** → the public key on the namespace
  does not match the `GPG_PRIVATE_KEY` used to sign. Re-check Phase 1 steps 2–3
  (same key pair).
- **Repo not selectable when connecting** → the Terraform Registry GitHub App
  lacks access to `Botyard-AI/terraform-provider-botyard`; grant it in the
  org's GitHub App settings.
- **Docs not showing on the provider page** → `docs/` must be committed in the
  standard layout (`index.md` + `resources/` + `data-sources/`); it is generated
  by `make docs` and enforced by CI's docs-drift check.
