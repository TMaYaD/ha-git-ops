# Changelog

## 0.6.1

- **Changing `repo_url`, `branch` or `subfolder` now takes effect —
  without losing last-applied.** Previously the existing clone in
  `/data/repo` was reused as-is: it kept fetching from the old origin
  with the old sparse checkout, and the old baseline was diffed against
  the new paths, so a moved config showed every file as deleted on one
  side and added on the other. On start the add-on now replaces a clone
  that tracks a different repo or subfolder, after snapshotting each
  file's last-applied content (raw from git, so `secrets.sops.yaml`
  stays encrypted) to `/data/baseline`. The snapshot stands in as
  last-applied until each file converges on the new target and is
  deleted once all have — so after a move the panel still shows whether
  git or HA changed a file, and a file new in the target reads as new
  there. A persistent notification says the target changed.
- A branch-only change keeps the baseline as is; a clone that is merely
  missing is re-cloned without touching it. Only when the applied commit
  is unreachable with nothing to carry over does the add-on baseline
  afresh at the new head (writing nothing, with a notification).
- The target the baseline belongs to is recorded in `state.json`; state
  from earlier versions is carried over when its clone was stale.
- A leading or trailing `/` in `subfolder` is ignored instead of
  breaking every git lookup.

## 0.6.0

- **Three-way panel.** Every file that needs a decision is one card
  comparing three versions: *last-applied* (the git content HA was last
  in sync with), *live* and *target* (the branch head). Toggle which
  versions are shown; diffs compare only the shown ones and fold away
  differences that exist only in a hidden one. Replaces the separate
  Incoming / Changed in HA / Conflicts sections, so a file changed on
  both sides — or missing live while git moved — is one card with a real
  diff instead of a full deletion plus a full addition.
- **Line-level staging into two buffers.** Gutter arrows stage the lines
  their bar spans: last-applied → live (revert), target → live (apply)
  and live → target (promote). Drag across text or click line numbers to
  stage part of a change. Staged lines show in the column they will be
  written to, boxed in their source colour with an undo badge; the
  sidebar lists both buffers with **Apply to live** and **Commit & push**.
  Promote takes applied live content only — a line staged into live must
  be applied before it can be promoted. Staging survives the other
  button: after a commit, staged live lines are re-derived on the new
  diff (and vice versa), so merging both sides of a file takes two
  clicks.
- Writes are refused (HTTP 409) if any of the three versions changed
  since the panel loaded them, so nothing reviewed can be stale; the
  config check, rollback, targeted reloads and restart flag are as before.
- Last-applied is tracked per file: a file whose live content matches
  target is in sync at the new head even while others still differ, and
  the global baseline fast-forwards once nothing is behind.
- `auto_apply` now applies only files that changed in git alone; files
  changed live are left for a human, as conflicts were before.
- The secrets file is diffed with values replaced by keyed hashes — keys
  and structure stay readable, equal values carry equal tags, and
  nothing secret reaches the browser.
- The panel is now a client-side page over a small JSON API
  (`api/state`, `api/apply`, `api/commit`, `api/refresh`);
  `go run ./cmd/preview` serves it against an in-memory fixture with
  working apply and commit.

## 0.5.1

- Prebuilt images: GitHub Actions builds per-arch images on each GitHub
  release and publishes them to `ghcr.io/tmayad/{arch}-ha-git-ops`.
  Installing or updating the add-on now pulls the image instead of
  compiling Go on the device.

## 0.5.0

- **Upstream commits are no longer applied sight unseen.** The poll loop
  is now read-only (fetch + diff): new commits surface in the panel as
  an "Incoming from git" section with per-file diffs of exactly what
  applying would change live, including a warning on files that apply
  would skip as conflicts. A single "Apply N files to HA" button writes
  the pending range — same config check, rollback, and never-stomp-
  local-edits guarantees as before. Commits that would change nothing
  live fast-forward the baseline silently.
- New options, ArgoCD semantics: `auto_refresh` (default `true`) —
  background fetch + diff; `auto_apply` (default `false`, ArgoCD's
  `syncPolicy.automated`) — apply incoming changes after each background
  refresh.
- `sensor.ha_gitops_drift` now counts drift + incoming + conflicts and
  exposes an `incoming` file-list attribute. The 0.4.0 drift
  notification also lists incoming files (except under `auto_apply`,
  where they resolve on their own and would only be push noise).
- Panel redesign: HA-native cards and palette, dark mode via
  `prefers-color-scheme`, diff syntax coloring, optional side-by-side
  (split) diff view persisted per browser, responsive layout, and an
  in-sync empty state.
- Copy pass — one verb per action, direction always explicit:
  "Apply git version" (was "Revert to git"), "Promote", "↻ Refresh"
  (was "Sync now"), drift chips "modified in HA" / "missing in HA" /
  "only in HA".
- Dev: `cmd/preview` renders the panel with fixture states
  (`go run ./cmd/preview`) for design iteration without a live stack.

## 0.4.0

- Drift notifications: a persistent notification now lists every file
  awaiting a human decision (drift and conflicts), updates when the set
  changes, and dismisses itself once back in sync. On by default;
  `notify_drift: false` turns it off.
- New `notify_service` option (e.g. `notify.mobile_app_phone`): every
  GitOps alert — new drift, conflicts, apply rollbacks, restart
  required — is additionally pushed through that service, so it reaches
  a phone instead of waiting in the panel.
- The "GitOps conflict" persistent notification is dismissed by the
  next conflict-free apply instead of lingering forever.

## 0.3.1

- Hide the Revert button on "live only" dashboard/registry drift — there
  is nothing in git to revert to and the guard refused it anyway; the
  panel now says "not in git yet — promote to adopt".

## 0.3.0

- Tier-3 custom representations: floors and labels. Live registries
  project to `registry/floors.yaml` / `registry/labels.yaml` keyed by
  slug with human-meaningful fields only (no ids/timestamps). Apply
  converges live per item — create/update/delete via the registry
  websocket APIs; fields absent in git are cleared. Deleting the whole
  file from git is refused. Empty registries don't surface. Item keys
  must be HA's slug of the name (lowercase, underscores).

## 0.2.2

- Replace post-action redirects with a small self-refreshing page — the
  HA ingress iframe gets stuck on "loading data" when a form POST
  answers with a 303.

## 0.2.1

- Promote is now self-healing: the worktree resets to the remote head
  before staging, removals use --ignore-unmatch, and an empty stage is a
  graceful no-op. A failed promote (e.g. rejected push) can no longer
  poison retries with leftover commits. Promote/revert errors are also
  logged, not just returned to the browser.

## 0.2.0

- Tier 2: storage-mode Lovelace dashboards. `.storage/lovelace.<id>`
  JSON ⇄ `dashboards/<url_path>.yaml` in git, compared in a canonical
  sorted-key YAML form (formatting/key order never cause drift). Apply
  and revert go through the websocket `lovelace/config/save` (no
  restart); promote writes the canonical YAML. Live dashboards not in
  git surface as "live only (promote to adopt)". Deleting dashboards
  from git is intentionally not applied.

## 0.1.2

- Skip files whose live content already matches the new desired state
  during apply (e.g. sops re-encryption with unchanged plaintext) —
  avoids spurious writes and false restart-required flags.

## 0.1.1

- Fix: s6-overlay v3 strips the container environment for CMD — read
  SUPERVISOR_TOKEN from /run/s6/container_environment/ as a fallback and
  pass HOME explicitly to sops. Fixes 401s on every core API call and
  sops home-directory warnings.

## 0.1.0

- Initial v0: apply loop (sparse checkout, sops decrypt, config check
  with rollback, targeted reload), Tier-1 drift detection, ingress panel
  with per-file diff and promote/revert, drift sensor, conflict
  surfacing. First run baselines without writing.
