# HA GitOps

## Setup

1. Install the add-on and set the options:
   - `repo_url` — ssh clone URL, e.g. `git@github.com:you/homelab.git`
   - `subfolder` — repo path that mirrors `/config`, e.g. `home-assistant/config`
   - `branch`, `poll_interval`, `auto_restart_core` as you like
   - `notify_drift`, `notify_service` — drift/conflict notifications
     and optional push routing (see Behavior below)
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
- Drift is announced, not just displayed: a persistent notification
  lists every file awaiting a decision, updates as the set changes, and
  dismisses itself once you're back in sync (`notify_drift: false`
  turns this off). Set `notify_service` to a notify service (e.g.
  `notify.mobile_app_your_phone`) and every GitOps alert — new drift,
  conflicts, rollbacks, restart required — is also pushed through it.
- `sensor.ha_gitops_drift` exposes the drift count with file lists as
  attributes — automate reminders off it if you like.
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
  never show as drift. Apply/revert save through the websocket API and
  take effect without a restart; live dashboards missing from git show
  as "live only (promote to adopt)". Deleting a dashboard file from git
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
