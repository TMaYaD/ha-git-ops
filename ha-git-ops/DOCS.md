# HA GitOps

## Setup

1. Install the add-on and set the options:
   - `repo_url` — ssh clone URL, e.g. `git@github.com:you/homelab.git`
   - `subfolder` — repo path that mirrors `/config`, e.g. `home-assistant/config`
   - `branch`, `poll_interval`, `auto_refresh`, `auto_apply`,
     `auto_restart_core` as you like
   - `notify_drift`, `notify_service` — notifications for files awaiting
     a decision, and optional push routing (see Behavior below)
2. Start it once. The log prints two public keys:
   - the **ssh deploy key** — add it to the git host with **write** access
     (write is needed for promote)
   - the **age public key** — append it to the repo's `.sops.yaml`
     recipients, then run `sops updatekeys <path>/secrets.sops.yaml`
     from a machine holding an existing recipient key, and commit
3. Restart the add-on. It clones (sparse checkout of `subfolder`) and
   **baselines without writing anything** — every difference between git
   and live shows up as drift in the ingress panel for you to converge
   deliberately, file by file: promote the HA version to git, or apply
   the git version. Converge that list once and you're in sync.

## Behavior

- ArgoCD semantics. **Refresh** (fetch + diff) is automatic and
  read-only: new commits show up in the panel as **incoming** changes
  with per-file diffs. **Apply** is manual by default — nothing is
  written sight unseen until you click it. Applying is sops-decrypted,
  config-checked (rolled back on failure), then activated via targeted
  reloads. `configuration.yaml`/`secrets.yaml` changes flag a restart
  instead (or restart automatically with `auto_restart_core: true`).
- `auto_apply: true` (ArgoCD's `syncPolicy.automated`) applies incoming
  changes after each background refresh, for those who trust their
  pipeline. `auto_refresh: false` disables background polling entirely —
  the panel's Refresh button becomes the only fetch. Defaults:
  `auto_refresh: true`, `auto_apply: false`.
- Commits that would change nothing live (e.g. a sops re-encryption to
  the same plaintext) fast-forward the baseline silently.
- A file changed **both** in git and live is a conflict: never
  auto-resolved, surfaced in the panel and as a persistent notification.
- Live (click-ops) changes are **never** committed automatically. The
  panel shows the diff; *promote* commits it to the branch with your
  message, *apply git version* restores the file from git, discarding
  the live change.
- Changes are announced, not just displayed: a persistent notification
  lists every file awaiting a decision (drift, incoming, and conflicts;
  incoming is left out under `auto_apply` since it resolves on its own),
  updates as the set changes, and dismisses itself once you're back in
  sync (`notify_drift: false` turns this off). Set `notify_service` to a
  notify service (e.g. `notify.mobile_app_your_phone`) and every GitOps
  alert — files awaiting a decision, conflicts, rollbacks, restart
  required — is also pushed through it.
- `sensor.ha_gitops_drift` counts everything needing attention (drift +
  incoming + conflicts) with file lists as attributes — automate
  reminders off it if you like.
- The secrets file is special-cased: git holds `secrets.sops.yaml`
  (values encrypted, keys plaintext), live holds `secrets.yaml`.
  Promote re-encrypts against all `.sops.yaml` recipients; diffs in the
  panel are masked.

## Files it manages

- Files tracked in git under `subfolder` (plus the `secrets.sops.yaml`
  → `secrets.yaml` translation). Untracked live files are ignored — add
  a file to git to bring it under management.
- **Storage-mode Lovelace dashboards**: `dashboards/<url_path>.yaml` in
  git maps to the live dashboard with that `url_path` (the unregistered
  default dashboard maps to `dashboards/default.yaml`). Comparison is on
  a canonical sorted-key YAML rendering, so formatting and key order
  never show as drift. Applying a git version saves through the
  websocket API and takes effect without a restart; live dashboards
  missing from git show as "only in HA" — promote to start tracking
  them. Deleting a dashboard file from git
  is deliberately not applied — delete the dashboard in the UI and
  promote instead.

- **Floors and labels**: `registry/floors.yaml` and
  `registry/labels.yaml` hold a map keyed by the HA slug with the
  human-meaningful fields (floors: name/level/icon/aliases; labels:
  name/icon/color/description) — ids and timestamps stay out of git.
  Apply converges the live registry item by item over the websocket API
  (create/update/delete; a field absent in git is cleared live).
  Deleting the whole file from git is refused as a registry-wipe guard;
  remove individual items instead. New item keys must match HA's slug
  of the name (lowercase, underscores). Empty registries don't surface.

Helpers and the remaining registry state are the next roadmap tiers;
see the repository README.
