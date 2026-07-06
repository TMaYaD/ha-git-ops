# HA GitOps

## Setup

1. Install the add-on and set the options:
   - `repo_url` — ssh clone URL, e.g. `git@github.com:you/homelab.git`
   - `subfolder` — repo path that mirrors `/config`, e.g. `home-assistant/config`
   - `branch`, `poll_interval`, `auto_restart_core` as you like
2. Start it once. The log prints two public keys:
   - the **ssh deploy key** — add it to the git host with **write** access
     (write is needed for promote)
   - the **age public key** — append it to the repo's `.sops.yaml`
     recipients, then run `sops updatekeys <path>/secrets.sops.yaml`
     from a machine holding an existing recipient key, and commit
3. Restart the add-on. It clones (sparse checkout of `subfolder`) and
   **baselines without writing anything** — every difference between git
   and live shows up as drift in the ingress panel for you to promote or
   revert deliberately. Converge that list once and you're in sync.

## Behavior

- New commits are applied automatically: sops-decrypted, config-checked
  (rolled back on failure), then activated via targeted reloads.
  `configuration.yaml`/`secrets.yaml` changes flag a restart instead
  (or restart automatically with `auto_restart_core: true`).
- A file changed **both** in git and live is a conflict: never
  auto-resolved, surfaced in the panel and as a persistent notification.
- Live (click-ops) changes are **never** committed automatically. The
  panel shows the diff; *promote* commits it to the branch with your
  message, *revert* restores the git version.
- `sensor.ha_gitops_drift` exposes the drift count with file lists as
  attributes — automate reminders off it if you like.
- The secrets file is special-cased: git holds `secrets.sops.yaml`
  (values encrypted, keys plaintext), live holds `secrets.yaml`.
  Promote re-encrypts against all `.sops.yaml` recipients; diffs in the
  panel are masked.

## Files it manages

Exactly the files tracked in git under `subfolder` (plus the
`secrets.sops.yaml` → `secrets.yaml` translation). Untracked live files
are ignored in v0 — add a file to git to bring it under management.
`.storage` translation (dashboards, helpers, registries) is the v1/v2
roadmap; see the repository README.
