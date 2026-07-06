"""Reconciler: git desired state vs live /config, ArgoCD-style.

State machine per managed file:
  in-sync    live == HEAD
  drift      live != HEAD, and HEAD did not move (click-ops change)
  conflict   HEAD moved AND live != the previously-applied version
             (both sides changed; never auto-resolved)

Apply only writes files whose live content still matches the previously
applied git version — local edits are never stomped. On first run nothing
is written at all: the remote HEAD becomes the baseline and every
difference shows up as drift for the human to promote or revert.
"""

import json
import logging
import shutil
import subprocess
import time
from pathlib import Path

log = logging.getLogger("reconciler")

SECRETS_SOPS = "secrets.sops.yaml"
SECRETS_PLAIN = "secrets.yaml"
# Files whose change requires a core restart rather than a reload.
RESTART_FILES = {"configuration.yaml", SECRETS_PLAIN}
RELOAD_SERVICES = {
    "automations.yaml": ("automation", "reload"),
    "scenes.yaml": ("scene", "reload"),
    "scripts.yaml": ("script", "reload"),
}


class Reconciler:
    def __init__(self, opts, ha):
        self.opts = opts
        self.ha = ha
        self.repo = Path("/data/repo")
        self.config = Path("/homeassistant")
        self.state_file = Path("/data/state.json")
        self.age_key = Path("/data/age.key")
        self.ssh_key = Path("/data/ssh/id_ed25519")
        self.backups = Path("/data/backups")
        self.state = self._load_state()
        # Computed each tick; read by the web UI.
        self.status = {"drift": {}, "conflicts": {}, "error": None,
                       "restart_required": False, "last_sync": None}

    # ---------- setup ----------

    def _load_state(self):
        if self.state_file.exists():
            return json.loads(self.state_file.read_text())
        return {"applied_sha": None}

    def _save_state(self):
        self.state_file.write_text(json.dumps(self.state))

    def ensure_keys(self):
        if not self.age_key.exists():
            subprocess.run(["age-keygen", "-o", str(self.age_key)], check=True)
            self.age_key.chmod(0o600)
        age_pub = subprocess.run(
            ["age-keygen", "-y", str(self.age_key)],
            check=True, capture_output=True, text=True).stdout.strip()
        if not self.ssh_key.exists():
            self.ssh_key.parent.mkdir(parents=True, exist_ok=True)
            subprocess.run(
                ["ssh-keygen", "-t", "ed25519", "-N", "", "-C", "ha-git-ops",
                 "-f", str(self.ssh_key)], check=True)
        ssh_pub = (self.ssh_key.with_suffix(".pub")).read_text().strip()
        self.pubkeys = {"age": age_pub, "ssh": ssh_pub}
        log.info("age public key (add to .sops.yaml recipients, then "
                 "`sops updatekeys`): %s", age_pub)
        log.info("ssh deploy key (add to the git host with write access): %s",
                 ssh_pub)

    # ---------- git plumbing ----------

    def _git(self, *args, check=True, input=None):
        env = {
            "GIT_SSH_COMMAND":
                f"ssh -i {self.ssh_key} -o StrictHostKeyChecking=accept-new",
            "HOME": "/data",
            "PATH": "/usr/local/bin:/usr/bin:/bin",
        }
        r = subprocess.run(["git", "-C", str(self.repo), *args],
                           capture_output=True, text=True, env=env, input=input)
        if check and r.returncode != 0:
            raise RuntimeError(f"git {' '.join(args)}: {r.stderr.strip()}")
        return r.stdout

    def ensure_repo(self):
        if (self.repo / ".git").exists():
            return
        if not self.opts["repo_url"]:
            raise RuntimeError("repo_url is not configured")
        self.repo.parent.mkdir(parents=True, exist_ok=True)
        env = {
            "GIT_SSH_COMMAND":
                f"ssh -i {self.ssh_key} -o StrictHostKeyChecking=accept-new",
            "HOME": "/data",
            "PATH": "/usr/local/bin:/usr/bin:/bin",
        }
        r = subprocess.run(
            ["git", "clone", "--filter=blob:none", "--no-checkout",
             "--branch", self.opts["branch"],
             self.opts["repo_url"], str(self.repo)],
            capture_output=True, text=True, env=env)
        if r.returncode != 0:
            raise RuntimeError(f"git clone: {r.stderr.strip()}")
        self._git("sparse-checkout", "set", self.opts["subfolder"])
        self._git("checkout", self.opts["branch"])
        self._git("config", "user.name", self.opts["commit_name"])
        self._git("config", "user.email", self.opts["commit_email"])

    def _tracked(self, ref):
        """Managed rel-paths (relative to subfolder) at a git ref."""
        sub = self.opts["subfolder"]
        out = self._git("ls-tree", "-r", "--name-only", ref, "--", sub)
        return [p[len(sub) + 1:] for p in out.splitlines() if p.strip()]

    def _at_ref(self, ref, rel):
        """File content at a git ref, or None. Decrypts the secrets file."""
        sub = self.opts["subfolder"]
        src = SECRETS_SOPS if rel == SECRETS_PLAIN else rel
        r = subprocess.run(
            ["git", "-C", str(self.repo), "show", f"{ref}:{sub}/{src}"],
            capture_output=True)
        if r.returncode != 0:
            return None
        if rel == SECRETS_PLAIN:
            return self._decrypt(r.stdout)
        return r.stdout

    def _decrypt(self, blob):
        r = subprocess.run(
            ["sops", "--decrypt", "--input-type", "yaml",
             "--output-type", "yaml", "/dev/stdin"],
            input=blob, capture_output=True,
            env={"SOPS_AGE_KEY_FILE": str(self.age_key),
                 "PATH": "/usr/local/bin:/usr/bin:/bin"})
        if r.returncode != 0:
            raise RuntimeError(f"sops decrypt failed: {r.stderr.decode()}")
        return r.stdout

    def desired_rels(self, ref):
        """Managed rel-paths as they appear live (sops name translated)."""
        rels = []
        for rel in self._tracked(ref):
            rels.append(SECRETS_PLAIN if rel == SECRETS_SOPS else rel)
        return rels

    def _live(self, rel):
        p = self.config / rel
        return p.read_bytes() if p.exists() else None

    # ---------- reconcile ----------

    async def tick(self):
        self.ensure_repo()
        self._git("fetch", "origin", self.opts["branch"])
        remote = self._git("rev-parse",
                           f"origin/{self.opts['branch']}").strip()
        applied = self.state["applied_sha"]

        if applied is None:
            # First run: baseline only, write nothing. Differences appear
            # as drift for the human to converge deliberately.
            self._git("reset", "--hard", remote)
            self.state["applied_sha"] = remote
            self._save_state()
            log.info("baselined at %s without applying (first run)", remote[:9])
        elif remote != applied:
            await self._apply(applied, remote)

        self._compute_drift()
        await self._publish()
        self.status["last_sync"] = time.strftime("%Y-%m-%d %H:%M:%S")

    async def _apply(self, old, new):
        """Apply old..new to /config, skipping conflicted files."""
        changed = [l.split("\t", 1) for l in self._git(
            "diff", "--name-status", old, new, "--",
            self.opts["subfolder"]).splitlines()]
        sub = self.opts["subfolder"]
        applied, conflicts = [], {}
        backup_dir = self.backups / time.strftime("%Y%m%d-%H%M%S")

        for status_, path in changed:
            rel = path[len(sub) + 1:]
            if rel == SECRETS_SOPS:
                rel = SECRETS_PLAIN
            live = self._live(rel)
            was = self._at_ref(old, rel)
            if live is not None and was is not None and live != was:
                conflicts[rel] = "changed in git and live"
                continue
            want = self._at_ref(new, rel)
            target = self.config / rel
            if live is not None:
                backup_dir.mkdir(parents=True, exist_ok=True)
                (backup_dir / rel.replace("/", "__")).write_bytes(live)
            if want is None:  # deleted in git
                if target.exists():
                    target.unlink()
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(want)
            applied.append(rel)

        self.status["conflicts"] = conflicts
        if conflicts:
            await self.ha.notify(
                "GitOps conflict",
                "Changed in both git and live, not applied: "
                + ", ".join(sorted(conflicts)))

        if applied:
            ok = await self.ha.core_check()
            if not ok:
                for rel in applied:  # roll back everything we wrote
                    saved = backup_dir / rel.replace("/", "__")
                    target = self.config / rel
                    if saved.exists():
                        target.write_bytes(saved.read_bytes())
                    elif target.exists():
                        target.unlink()
                await self.ha.notify(
                    "GitOps apply rolled back",
                    f"{old[:9]}..{new[:9]} failed core config check; "
                    "live files restored.")
                self.status["error"] = "config check failed; rolled back"
                return
            await self._activate(applied)
            log.info("applied %s..%s: %s", old[:9], new[:9], applied)

        self.state["applied_sha"] = new
        self._save_state()
        self._git("reset", "--hard", new)

    async def _activate(self, rels):
        """Reload what we can; flag a restart for what we can't."""
        if any(r in RESTART_FILES for r in rels):
            if self.opts["auto_restart_core"]:
                await self.ha.core_restart()
                self.status["restart_required"] = False
            else:
                self.status["restart_required"] = True
                await self.ha.notify(
                    "GitOps: restart required",
                    "configuration.yaml or secrets.yaml changed; restart "
                    "Home Assistant to activate.")
            return
        reloaded = set()
        for rel in rels:
            svc = RELOAD_SERVICES.get(rel)
            if svc and svc not in reloaded:
                await self.ha.call_service(*svc)
                reloaded.add(svc)
        if not all(RELOAD_SERVICES.get(r) for r in rels):
            await self.ha.call_service("homeassistant", "reload_all")

    def _compute_drift(self):
        head = self.state["applied_sha"]
        drift = {}
        for rel in self.desired_rels(head):
            want = self._at_ref(head, rel)
            live = self._live(rel)
            if live is None:
                drift[rel] = "missing live"
            elif live != want:
                drift[rel] = "modified live"
        self.status["drift"] = drift

    async def _publish(self):
        n = len(self.status["drift"]) + len(self.status["conflicts"])
        await self.ha.set_state(
            "sensor.ha_gitops_drift", n,
            {"friendly_name": "GitOps drift", "icon": "mdi:source-branch-sync",
             "drift": sorted(self.status["drift"]),
             "conflicts": sorted(self.status["conflicts"]),
             "applied_sha": (self.state["applied_sha"] or "")[:9],
             "restart_required": self.status["restart_required"]})

    # ---------- human actions (from the web UI) ----------

    async def revert(self, rel):
        want = self._at_ref(self.state["applied_sha"], rel)
        target = self.config / rel
        if want is None:
            if target.exists():
                target.unlink()
        else:
            target.write_bytes(want)
        await self._activate([rel])
        self._compute_drift()
        await self._publish()

    async def promote(self, rels, message):
        sub = self.opts["subfolder"]
        for rel in rels:
            live = self._live(rel)
            if live is None:
                self._git("rm", "-q", f"{sub}/{rel}")
                continue
            if rel == SECRETS_PLAIN:
                # Re-encrypt with the repo's .sops.yaml recipients so the
                # operator key can still decrypt. Rules match by path.
                out = self.repo / sub / SECRETS_SOPS
                out.write_bytes(live)
                r = subprocess.run(
                    ["sops", "--encrypt", "--in-place", str(out)],
                    capture_output=True, cwd=str(self.repo),
                    env={"PATH": "/usr/local/bin:/usr/bin:/bin"})
                if r.returncode != 0:
                    raise RuntimeError(f"sops encrypt: {r.stderr.decode()}")
                self._git("add", f"{sub}/{SECRETS_SOPS}")
                continue
            target = self.repo / sub / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(live)
            self._git("add", f"{sub}/{rel}")
        self._git("commit", "-m", message or f"promote: {', '.join(rels)}")
        try:
            self._git("pull", "--rebase", "origin", self.opts["branch"])
        except RuntimeError:
            self._git("rebase", "--abort", check=False)
            raise RuntimeError(
                "promote conflicts with new upstream commits; sync first")
        self._git("push", "origin", f"HEAD:{self.opts['branch']}")
        self.state["applied_sha"] = self._git("rev-parse", "HEAD").strip()
        self._save_state()
        self._compute_drift()
        await self._publish()
