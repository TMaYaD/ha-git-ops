<p align="center">
  <img src="ha-git-ops/logo.png" alt="ha-git-ops" width="500">
</p>

ArgoCD-style GitOps for Home Assistant OS.

Git is desired state. The HA instance is live state. This add-on applies
new commits to `/config`, continuously surfaces drift (click-ops changes),
and lets a human decide per file: **revert** to git, or **promote** to git.
It never auto-commits.

Secrets are SOPS + age encrypted at value level (`secrets.sops.yaml` in
git ⇄ plaintext `secrets.yaml` on the instance). The add-on generates its
own age key on first start, sealed-secrets style — the private key never
leaves the machine.

## Install

Add this repository URL in **Settings → Add-ons → Add-on store → ⋮ →
Repositories**, then install *HA GitOps*. See
[ha-git-ops/DOCS.md](ha-git-ops/DOCS.md) for setup.

## Design

Born from [TMaYaD/homelab](https://github.com/TMaYaD/homelab) —
the full strategy (tier model for `.storage` translation, BCDR story,
decisions log) lives in that repo's `home-assistant/GITOPS.md`.

Roadmap:

- **v0** — apply loop + Tier 1 drift (plain YAML files) + ingress UI with
  per-file diff, promote/revert. ✓ 0.1.x
- **v1** — Tier 2: `.storage` JSON with faithful YAML forms translated to
  canonical YAML projections. Storage-mode dashboards ✓ 0.2.0; helpers
  next.
- **v2** — Tier 3: baselines for registry-only state, plus custom
  deterministic representations for meaningful values (floors, labels,
  HACS manifest).
