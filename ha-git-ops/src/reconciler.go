package main

// Reconciler state machine per managed file:
//
//	in-sync    live == HEAD
//	drift      live != HEAD and HEAD did not move (click-ops change)
//	conflict   HEAD moved AND live != previously-applied version
//	           (both sides changed; never auto-resolved)
//
// Apply only writes files whose live content still matches the previously
// applied git version — local edits are never stomped. On first run
// nothing is written: remote HEAD becomes the baseline and every
// difference shows up as drift for the human to promote or revert.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	secretsSops  = "secrets.sops.yaml"
	secretsPlain = "secrets.yaml"
)

// Files whose change requires a core restart rather than a reload.
var restartFiles = map[string]bool{
	"configuration.yaml": true,
	secretsPlain:         true,
}

var reloadServices = map[string][2]string{
	"automations.yaml": {"automation", "reload"},
	"scenes.yaml":      {"scene", "reload"},
	"scripts.yaml":     {"script", "reload"},
}

type Status struct {
	Drift           map[string]string
	Conflicts       map[string]string
	Error           string
	RestartRequired bool
	LastSync        string
}

type persistedState struct {
	AppliedSHA string `json:"applied_sha"`
}

type Reconciler struct {
	opts       Options
	ha         *HA
	repoDir    string
	configDir  string
	stateFile  string
	ageKeyFile string
	sshKeyFile string
	backupsDir string

	AgePub string
	SSHPub string

	mu     sync.Mutex
	state  persistedState
	status Status
}

func NewReconciler(opts Options, ha *HA) *Reconciler {
	r := &Reconciler{
		opts:       opts,
		ha:         ha,
		repoDir:    "/data/repo",
		configDir:  "/homeassistant",
		stateFile:  "/data/state.json",
		ageKeyFile: "/data/age.key",
		sshKeyFile: "/data/ssh/id_ed25519",
		backupsDir: "/data/backups",
		status: Status{
			Drift:     map[string]string{},
			Conflicts: map[string]string{},
		},
	}
	if raw, err := os.ReadFile(r.stateFile); err == nil {
		_ = json.Unmarshal(raw, &r.state)
	}
	return r
}

// Snapshot returns a copy of the current status and applied SHA for the UI.
func (r *Reconciler) Snapshot() (Status, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.Drift = copyMap(r.status.Drift)
	st.Conflicts = copyMap(r.status.Conflicts)
	return st, r.state.AppliedSHA
}

func (r *Reconciler) SetError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Error = msg
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *Reconciler) saveState() error {
	raw, _ := json.Marshal(r.state)
	return os.WriteFile(r.stateFile, raw, 0o644)
}

// ---------- setup ----------

func (r *Reconciler) EnsureKeys() error {
	if _, err := os.Stat(r.ageKeyFile); err != nil {
		if out, err := exec.Command("age-keygen", "-o", r.ageKeyFile).CombinedOutput(); err != nil {
			return fmt.Errorf("age-keygen: %v: %s", err, out)
		}
		_ = os.Chmod(r.ageKeyFile, 0o600)
	}
	pub, err := exec.Command("age-keygen", "-y", r.ageKeyFile).Output()
	if err != nil {
		return fmt.Errorf("age-keygen -y: %v", err)
	}
	r.AgePub = strings.TrimSpace(string(pub))

	if _, err := os.Stat(r.sshKeyFile); err != nil {
		if err := os.MkdirAll(filepath.Dir(r.sshKeyFile), 0o700); err != nil {
			return err
		}
		if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "",
			"-C", "ha-git-ops", "-f", r.sshKeyFile).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	sshPub, err := os.ReadFile(r.sshKeyFile + ".pub")
	if err != nil {
		return err
	}
	r.SSHPub = strings.TrimSpace(string(sshPub))

	log.Printf("age public key (add to .sops.yaml recipients, then `sops updatekeys`): %s", r.AgePub)
	log.Printf("ssh deploy key (add to the git host with write access): %s", r.SSHPub)
	return nil
}

// ---------- git plumbing ----------

func (r *Reconciler) gitEnv() []string {
	return append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -i "+r.sshKeyFile+" -o StrictHostKeyChecking=accept-new",
		"HOME=/data",
	)
}

func (r *Reconciler) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.repoDir}, args...)...)
	cmd.Env = r.gitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "),
			strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (r *Reconciler) ensureRepo() error {
	if _, err := os.Stat(filepath.Join(r.repoDir, ".git")); err == nil {
		return nil
	}
	if r.opts.RepoURL == "" {
		return fmt.Errorf("repo_url is not configured")
	}
	cmd := exec.Command("git", "clone", "--filter=blob:none", "--no-checkout",
		"--branch", r.opts.Branch, r.opts.RepoURL, r.repoDir)
	cmd.Env = r.gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"sparse-checkout", "set", r.opts.Subfolder},
		{"checkout", r.opts.Branch},
		{"config", "user.name", r.opts.CommitName},
		{"config", "user.email", r.opts.CommitEmail},
	} {
		if _, err := r.git(args...); err != nil {
			return err
		}
	}
	return nil
}

// atRef returns desired content at a git ref (nil if absent), in the
// same shape live() produces: the secrets file decrypted, dashboards
// canonicalized.
func (r *Reconciler) atRef(ref, rel string) []byte {
	src := rel
	if rel == secretsPlain {
		src = secretsSops
	}
	cmd := exec.Command("git", "-C", r.repoDir, "show",
		ref+":"+r.opts.Subfolder+"/"+src)
	cmd.Env = r.gitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	if rel == secretsPlain {
		dec, err := r.decrypt(out)
		if err != nil {
			log.Printf("sops decrypt failed: %v", err)
			return nil
		}
		return dec
	}
	if isDashboard(rel) || isRegistry(rel) {
		canon, err := CanonYAML(out)
		if err != nil {
			log.Printf("canonicalize %s@%.9s failed: %v", rel, ref, err)
			return nil
		}
		return canon
	}
	return out
}

func (r *Reconciler) decrypt(blob []byte) ([]byte, error) {
	cmd := exec.Command("sops", "--decrypt",
		"--input-type", "yaml", "--output-type", "yaml", "/dev/stdin")
	cmd.Env = append(os.Environ(),
		"SOPS_AGE_KEY_FILE="+r.ageKeyFile, "HOME=/data")
	cmd.Stdin = bytes.NewReader(blob)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// managedRels is the union of rel-paths tracked in git at ref (in
// live-file naming) and live storage-mode dashboards, so click-ops
// dashboards not yet in git surface as adoptable drift.
func (r *Reconciler) managedRels(ref string) ([]string, error) {
	out, err := r.git("ls-tree", "-r", "--name-only", ref, "--", r.opts.Subfolder)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var rels []string
	prefix := r.opts.Subfolder + "/"
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if p == "" {
			continue
		}
		rel := strings.TrimPrefix(p, prefix)
		if rel == secretsSops {
			rel = secretsPlain
		}
		seen[rel] = true
		rels = append(rels, rel)
	}
	dbs, err := r.liveDashboards()
	if err != nil {
		return nil, err
	}
	for rel := range dbs {
		if !seen[rel] {
			rels = append(rels, rel)
		}
	}
	for _, def := range registries {
		if !seen[def.rel] && r.registryLive(def.rel) != nil {
			rels = append(rels, def.rel)
		}
	}
	sort.Strings(rels)
	return rels, nil
}

func (r *Reconciler) live(rel string) []byte {
	if isDashboard(rel) {
		return r.dashboardLive(rel)
	}
	if isRegistry(rel) {
		return r.registryLive(rel)
	}
	b, err := os.ReadFile(filepath.Join(r.configDir, rel))
	if err != nil {
		return nil
	}
	return b
}

// writeLive materializes desired content on the live side; nil content
// means deletion.
func (r *Reconciler) writeLive(rel string, content []byte) error {
	if isDashboard(rel) {
		return r.dashboardWrite(rel, content)
	}
	if isRegistry(rel) {
		return r.registryWrite(rel, content)
	}
	target := filepath.Join(r.configDir, rel)
	if content == nil {
		return os.Remove(target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o644)
}

// ---------- reconcile ----------

func (r *Reconciler) Tick() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tickLocked()
}

func (r *Reconciler) tickLocked() error {
	if err := r.ensureRepo(); err != nil {
		return err
	}
	if _, err := r.git("fetch", "origin", r.opts.Branch); err != nil {
		return err
	}
	remote, err := r.git("rev-parse", "origin/"+r.opts.Branch)
	if err != nil {
		return err
	}
	remote = strings.TrimSpace(remote)

	switch {
	case r.state.AppliedSHA == "":
		// First run: baseline only, write nothing. Differences appear as
		// drift for the human to converge deliberately.
		if _, err := r.git("reset", "--hard", remote); err != nil {
			return err
		}
		r.state.AppliedSHA = remote
		if err := r.saveState(); err != nil {
			return err
		}
		log.Printf("baselined at %.9s without applying (first run)", remote)
	case remote != r.state.AppliedSHA:
		if err := r.apply(r.state.AppliedSHA, remote); err != nil {
			return err
		}
	}

	if err := r.computeDrift(); err != nil {
		return err
	}
	r.publish()
	r.status.Error = ""
	r.status.LastSync = time.Now().Format("2006-01-02 15:04:05")
	return nil
}

// changedRels parses old..new into live-named rel paths (renames become
// a delete plus an add).
func (r *Reconciler) changedRels(old, new_ string) ([]string, error) {
	out, err := r.git("diff", "--name-status", old, new_, "--", r.opts.Subfolder)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var rels []string
	prefix := r.opts.Subfolder + "/"
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		for _, p := range fields[1:] { // status, path [, path for renames]
			rel := strings.TrimPrefix(p, prefix)
			if rel == secretsSops {
				rel = secretsPlain
			}
			if !seen[rel] {
				seen[rel] = true
				rels = append(rels, rel)
			}
		}
	}
	return rels, nil
}

func (r *Reconciler) apply(old, new_ string) error {
	rels, err := r.changedRels(old, new_)
	if err != nil {
		return err
	}
	var applied []string
	conflicts := map[string]string{}
	backupDir := filepath.Join(r.backupsDir, time.Now().Format("20060102-150405"))

	for _, rel := range rels {
		liveB := r.live(rel)
		was := r.atRef(old, rel)
		if liveB != nil && was != nil && !bytes.Equal(liveB, was) {
			conflicts[rel] = "changed in git and live"
			continue
		}
		want := r.atRef(new_, rel)
		if want != nil && liveB != nil && bytes.Equal(liveB, want) {
			// Live already has the desired content — e.g. a sops
			// re-encryption that decrypts to the same plaintext. No
			// write, no reload/restart.
			continue
		}
		if liveB != nil {
			if err := os.MkdirAll(backupDir, 0o700); err != nil {
				return err
			}
			name := strings.ReplaceAll(rel, "/", "__")
			if err := os.WriteFile(filepath.Join(backupDir, name), liveB, 0o600); err != nil {
				return err
			}
		}
		if err := r.writeLive(rel, want); err != nil {
			log.Printf("apply %s: %v", rel, err)
			conflicts[rel] = err.Error()
			continue
		}
		applied = append(applied, rel)
	}

	r.status.Conflicts = conflicts
	if len(conflicts) > 0 {
		keys := sortedKeys(conflicts)
		r.ha.Notify("GitOps conflict",
			"Changed in both git and live, not applied: "+strings.Join(keys, ", "))
	}

	if len(applied) > 0 {
		ok, err := r.ha.CoreCheck()
		if err != nil {
			return err
		}
		if !ok {
			for _, rel := range applied { // roll back everything we wrote
				name := strings.ReplaceAll(rel, "/", "__")
				saved, err := os.ReadFile(filepath.Join(backupDir, name))
				if err != nil {
					saved = nil
				}
				if err := r.writeLive(rel, saved); err != nil {
					log.Printf("rollback %s: %v", rel, err)
				}
			}
			r.ha.Notify("GitOps apply rolled back", fmt.Sprintf(
				"%.9s..%.9s failed core config check; live files restored.",
				old, new_))
			r.status.Error = "config check failed; rolled back"
			return nil
		}
		r.activate(applied)
		log.Printf("applied %.9s..%.9s: %v", old, new_, applied)
	}

	r.state.AppliedSHA = new_
	if err := r.saveState(); err != nil {
		return err
	}
	_, err = r.git("reset", "--hard", new_)
	return err
}

// activate reloads what it can and flags a restart for what it can't.
// Dashboards and registries need nothing: websocket writes are
// immediately live.
func (r *Reconciler) activate(all []string) {
	var rels []string
	for _, rel := range all {
		if !isDashboard(rel) && !isRegistry(rel) {
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return
	}
	needsRestart := false
	for _, rel := range rels {
		if restartFiles[rel] {
			needsRestart = true
		}
	}
	if needsRestart {
		if r.opts.AutoRestartCore {
			r.ha.CoreRestart()
			r.status.RestartRequired = false
		} else {
			r.status.RestartRequired = true
			r.ha.Notify("GitOps: restart required",
				"configuration.yaml or secrets.yaml changed; restart Home Assistant to activate.")
		}
		return
	}
	reloaded := map[[2]string]bool{}
	allMapped := true
	for _, rel := range rels {
		svc, ok := reloadServices[rel]
		if !ok {
			allMapped = false
			continue
		}
		if !reloaded[svc] {
			r.ha.CallService(svc[0], svc[1], nil)
			reloaded[svc] = true
		}
	}
	if !allMapped {
		r.ha.CallService("homeassistant", "reload_all", nil)
	}
}

func (r *Reconciler) computeDrift() error {
	head := r.state.AppliedSHA
	rels, err := r.managedRels(head)
	if err != nil {
		return err
	}
	drift := map[string]string{}
	for _, rel := range rels {
		want := r.atRef(head, rel)
		liveB := r.live(rel)
		switch {
		case want == nil && liveB == nil:
			// registered dashboard with no config yet, or vanished both
			// sides — nothing to reconcile
		case want == nil:
			drift[rel] = "live only (promote to adopt)"
		case liveB == nil:
			drift[rel] = "missing live"
		case !bytes.Equal(liveB, want):
			drift[rel] = "modified live"
		}
	}
	r.status.Drift = drift
	return nil
}

func (r *Reconciler) publish() {
	n := len(r.status.Drift) + len(r.status.Conflicts)
	sha := r.state.AppliedSHA
	if len(sha) > 9 {
		sha = sha[:9]
	}
	r.ha.SetState("sensor.ha_gitops_drift", fmt.Sprint(n), map[string]any{
		"friendly_name":    "GitOps drift",
		"icon":             "mdi:source-branch-sync",
		"drift":            sortedKeys(r.status.Drift),
		"conflicts":        sortedKeys(r.status.Conflicts),
		"applied_sha":      sha,
		"restart_required": r.status.RestartRequired,
	})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------- human actions (from the web UI) ----------

func (r *Reconciler) SyncNow() error { return r.Tick() }

func (r *Reconciler) Revert(rel string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := r.atRef(r.state.AppliedSHA, rel)
	if err := r.writeLive(rel, want); err != nil {
		return err
	}
	r.activate([]string{rel})
	if err := r.computeDrift(); err != nil {
		return err
	}
	r.publish()
	return nil
}

func (r *Reconciler) Promote(rels []string, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Start from a clean worktree at the remote head so leftovers from a
	// previously failed promote (staged files, unpushed commits) can't
	// poison this attempt. Live state is the source of truth for what
	// gets promoted, so nothing is lost by discarding local repo state.
	if _, err := r.git("fetch", "origin", r.opts.Branch); err != nil {
		return err
	}
	if _, err := r.git("reset", "--hard", "origin/"+r.opts.Branch); err != nil {
		return err
	}
	for _, rel := range rels {
		liveB := r.live(rel)
		if liveB == nil {
			if _, err := r.git("rm", "-q", "--ignore-unmatch",
				r.opts.Subfolder+"/"+rel); err != nil {
				return err
			}
			continue
		}
		if rel == secretsPlain {
			// Re-encrypt with the repo's .sops.yaml recipients so the
			// operator key can still decrypt. Rules match by path.
			out := filepath.Join(r.repoDir, r.opts.Subfolder, secretsSops)
			if err := os.WriteFile(out, liveB, 0o600); err != nil {
				return err
			}
			cmd := exec.Command("sops", "--encrypt", "--in-place", out)
			cmd.Dir = r.repoDir
			cmd.Env = append(os.Environ(), "HOME=/data")
			if o, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("sops encrypt: %v: %s", err, o)
			}
			if _, err := r.git("add", r.opts.Subfolder+"/"+secretsSops); err != nil {
				return err
			}
			continue
		}
		target := filepath.Join(r.repoDir, r.opts.Subfolder, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, liveB, 0o644); err != nil {
			return err
		}
		if _, err := r.git("add", r.opts.Subfolder+"/"+rel); err != nil {
			return err
		}
	}
	if _, err := r.git("diff", "--cached", "--quiet"); err == nil {
		// Nothing staged — repo already matches live (e.g. a retried
		// promote that had in fact been pushed). Recompute and move on.
		log.Printf("promote: nothing to commit for %v", rels)
	} else {
		if message == "" {
			message = "promote: " + strings.Join(rels, ", ")
		}
		if _, err := r.git("commit", "-m", message); err != nil {
			return err
		}
		if _, err := r.git("push", "origin", "HEAD:"+r.opts.Branch); err != nil {
			_, _ = r.git("reset", "--hard", "origin/"+r.opts.Branch)
			return fmt.Errorf("push failed, worktree reset — sync and retry: %v", err)
		}
	}
	head, err := r.git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	r.state.AppliedSHA = strings.TrimSpace(head)
	if err := r.saveState(); err != nil {
		return err
	}
	if err := r.computeDrift(); err != nil {
		return err
	}
	r.publish()
	return nil
}

// DiffText renders a unified diff of git (desired) vs live for the UI.
// Secrets are masked to avoid leaking values into the panel.
func (r *Reconciler) DiffText(rel string) string {
	if rel == secretsPlain {
		return "(secrets diff masked — values changed)"
	}
	r.mu.Lock()
	head := r.state.AppliedSHA
	r.mu.Unlock()

	tmpFile := func(prefix string, content []byte) (string, error) {
		if content == nil {
			return os.DevNull, nil
		}
		tmp, err := os.CreateTemp("", prefix)
		if err != nil {
			return "", err
		}
		_, werr := tmp.Write(content)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			return "", fmt.Errorf("write temp: %v %v", werr, cerr)
		}
		return tmp.Name(), nil
	}
	wantPath, err := tmpFile("desired-*", r.atRef(head, rel))
	if err != nil {
		return err.Error()
	}
	if wantPath != os.DevNull {
		defer os.Remove(wantPath)
	}
	livePath, err := tmpFile("live-*", r.live(rel))
	if err != nil {
		return err.Error()
	}
	if livePath != os.DevNull {
		defer os.Remove(livePath)
	}
	// git diff --no-index exits 1 when files differ; that's expected.
	cmd := exec.Command("git", "diff", "--no-index",
		"--src-prefix=git/", "--dst-prefix=live/", wantPath, livePath)
	out, _ := cmd.Output()
	return string(out)
}
