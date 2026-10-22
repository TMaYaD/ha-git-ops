# HA GitOps

## Setup

1. Install the add-on and set the options:
   - `repo_url` — ssh clone URL, e.g. `git@github.com:you/homelab.git`
   - `subfolder` — repo path that mirrors `/config`, e.g. `home-assistant/config`
   - `branch`, `poll_interval`, `auto_refresh`, `auto_apply`,
     `auto_restart_core` as you like
   - Changing `repo_url`, `branch` or `subfolder` later (say, after moving
     the config to another repo) takes effect on the next start: the
     clone is replaced, and each file keeps what was last applied from the
     previous target — even if that repo no longer exists — until it
     converges on the new one. Nothing is written.
   - `commit_branch` — where **Commit & push** goes by default; empty
     commits straight to `branch`. Set it (e.g. `gitops/drift`) to raise
     pull requests instead of committing to the tracked branch — see
     *Committing to another branch* below
   - `notify_drift`, `notify_service` — notifications for files awaiting
     a decision, and optional push routing (see Behavior below)
2. Start it once. The log prints two public keys:
   - the **ssh deploy key** — add it to the git host with **write** access
     (write is needed for commits). On GitHub a deploy key belongs to one
     repository: if you later move the config to another repo, remove the
     key from the old one and add it to the new one.
   - the **age public key** — append it to the repo's `.sops.yaml`
     recipients, then run `sops updatekeys <path>/secrets.sops.yaml`
     from a machine holding an existing recipient key, and commit
3. Restart the add-on. It clones (sparse checkout of `subfolder`) and
   **baselines without writing anything** — every difference between git
   and live shows up in the ingress panel for you to converge
   deliberately: stage HA's lines into git or git's lines into HA.
   Converge that list once and you're in sync.

## The panel

Every file whose live content differs from git is one card comparing
three versions:

- **last-applied** — the git content HA was last in sync with
- **live** — what Home Assistant runs now
- **target** — the head of `branch`

A file is *live changed*, *target changed* or *both changed* depending
on which side moved away from last-applied. The version cards toggle
which columns are shown (at least two); diffs compare only the shown
versions, and differences that exist only in a hidden one are folded
away. Live vs target is the default.

Live and target are **buffers**. Arrows in the gutters stage the lines
their bar spans:

- **|→ last-applied → live** — revert a live change
- **←| target → live** — apply a git change
- **|→ live → target** — promote a live change

Drag across lines (or click line numbers, shift-click for a range) to
stage part of a change — the arrows split around the selection. Staged
lines appear in the column they will be written to, boxed in their
source colour, with the replaced lines struck through and an undo badge
on the box. Promote only takes applied live content: a line staged into
live must be applied before it can be promoted.

The sidebar lists both buffers. **Apply to live** writes the live buffer
— sops-decrypted, config-checked (rolled back on failure), then activated
via targeted reloads; `configuration.yaml`/`secrets.yaml` changes flag a
restart instead (or restart automatically with `auto_restart_core:
true`). **Commit & push** commits the target buffer with your message.
Staging in the other buffer survives either action. A write is refused
if any of the three versions changed since the panel loaded them —
refresh and review again.

### Committing to another branch

The target buffer has a branch field, defaulting to `commit_branch` (or
`branch` when that is empty). Name any other branch — existing or new —
to propose the change there, e.g. for a pull request into `branch`,
instead of committing to `branch` directly:

- The commit is always built on the head of `branch`. A new branch, or
  one `branch` already contains (a merged PR), starts from it.
- An open branch keeps what it already proposes: the change is merged
  onto it, so promoting more lines later grows the same PR instead of
  reverting earlier commits. If the branch already changes the same lines
  differently the commit is refused — use a new branch, or merge or
  delete the old one first.
- `branch` stays the target, so the promoted lines keep showing as live
  changes (and in the drift notification) until the PR is merged; the
  next refresh after the merge finds them in sync.

## Behavior

- ArgoCD semantics. **Refresh** (fetch + diff) is automatic and
  read-only: new commits show up in the panel as target changes. Nothing
  is written sight unseen until you apply or commit.
- `auto_apply: true` (ArgoCD's `syncPolicy.automated`) applies files
  that changed only in git after each background refresh, for those who
  trust their pipeline; files changed live are left for you.
  `auto_refresh: false` disables background polling entirely — the
  panel's Refresh button becomes the only fetch. Defaults:
  `auto_refresh: true`, `auto_apply: false`.
- Last-applied is tracked per file and stored on disk (`/data/baseline`)
  when the file converges: a file whose live content matches target is in
  sync at the new head even while others still differ, and last-applied
  does not depend on git history staying reachable.
  Commits that would change nothing live (e.g. a sops re-encryption to
  the same plaintext) fast-forward the baseline silently.
- A file changed **both** in git and live is never auto-resolved; you
  merge it line by line in the panel.
- Live (click-ops) changes are **never** committed automatically.
- Changes are announced, not just displayed: a persistent notification
  lists every file awaiting a decision (changes in git are left out
  under `auto_apply` since they resolve on their own),
  updates as the set changes, and dismisses itself once you're back in
  sync (`notify_drift: false` turns this off). Set `notify_service` to a
  notify service (e.g. `notify.mobile_app_your_phone`) and every GitOps
  alert — files awaiting a decision, conflicts, rollbacks, restart
  required — is also pushed through it.
- `sensor.ha_gitops_drift` counts every file needing attention, with
  file lists as attributes (`drift` = live changed, `incoming` = target
  changed, `conflicts` = both changed) — automate reminders off it if
  you like.
- The secrets file is special-cased: git holds `secrets.sops.yaml`
  (values encrypted, keys plaintext), live holds `secrets.yaml`.
  Commits re-encrypt against all `.sops.yaml` recipients. In the panel
  each value is replaced by a short keyed hash — keys stay readable and
  equal values carry equal tags, but no secret reaches the browser.

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
  missing from git show as live changes — commit them to start
  tracking. Deleting a dashboard file from git
  is deliberately not applied — delete the dashboard in the UI and
  commit instead.

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
