package main

// Every managed file has three versions:
//
//	last-applied  the git content HA was last in sync with (per file)
//	live          what Home Assistant runs now
//	target        the branch head, fetched
//
// A file needs a decision when live != target; it is "live changed",
// "target changed" or "both changed" depending on which side moved away
// from last-applied. The panel stages line-level picks into two buffers
// — live (from last-applied or target) and target (from applied live
// content) — and writes them only on an explicit apply or commit.
//
// Nothing is ever written sight unseen: the poll loop only fetches and
// recomputes. Apply and commit carry a token of the three versions the
// human reviewed and are refused if any of them changed since. On first
// run nothing is written: remote HEAD becomes the baseline and every
// difference shows up for the human to converge deliberately.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TMaYaD/ha-git-ops/diff3"
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

// File kinds: which side moved away from last-applied.
const (
	kindLive   = "live"
	kindTarget = "target"
	kindBoth   = "both"
)

var (
	// ErrStale refuses a write built on versions that changed since the
	// panel loaded them.
	ErrStale = errors.New("changed since the panel loaded it — refresh and review again")
	// ErrRolledBack reports an apply undone by a failed config check.
	ErrRolledBack = errors.New("core config check failed; live files restored")
)

// FileState is one file that needs a decision.
type FileState struct {
	Rel                string
	Kind               string
	BaseSHA            string
	Base, Live, Target []byte
	Rows               []diff3.Row
	Token              string
}

type Status struct {
	Files           []FileState
	TargetSHA       string
	Error           string
	RestartRequired bool
	LastSync        string
}

type persistedState struct {
	AppliedSHA string `json:"applied_sha"`
	// Files that converged on a newer target while others still differ
	// keep their own baseline until AppliedSHA catches up.
	Baselines map[string]string `json:"baselines,omitempty"`
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

	mu       sync.Mutex
	state    persistedState
	status   Status
	notified map[string]bool // rels covered by the current drift notification
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
	}
	if raw, err := os.ReadFile(r.stateFile); err == nil {
		_ = json.Unmarshal(raw, &r.state)
	}
	return r
}

// Snapshot returns a copy of the current status, the applied SHA and how
// many files carry their own newer baseline.
func (r *Reconciler) Snapshot() (Status, string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.Files = append([]FileState(nil), r.status.Files...)
	return st, r.state.AppliedSHA, len(r.state.Baselines)
}

func (r *Reconciler) SetError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Error = msg
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

// fetchRemote fetches the branch and returns its head.
func (r *Reconciler) fetchRemote() (string, error) {
	if err := r.ensureRepo(); err != nil {
		return "", err
	}
	if _, err := r.git("fetch", "origin", r.opts.Branch); err != nil {
		return "", err
	}
	remote, err := r.git("rev-parse", "origin/"+r.opts.Branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(remote), nil
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

// writeRepo stages content for rel in the repo worktree; nil removes it.
// The secrets file is re-encrypted with the repo's .sops.yaml recipients
// so the operator key can still decrypt; rules match by path.
func (r *Reconciler) writeRepo(rel string, content []byte) error {
	src := rel
	if rel == secretsPlain {
		src = secretsSops
	}
	gitPath := r.opts.Subfolder + "/" + src
	if content == nil {
		_, err := r.git("rm", "-q", "--ignore-unmatch", gitPath)
		return err
	}
	out := filepath.Join(r.repoDir, r.opts.Subfolder, src)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if rel == secretsPlain {
		if err := os.WriteFile(out, content, 0o600); err != nil {
			return err
		}
		cmd := exec.Command("sops", "--encrypt", "--in-place", out)
		cmd.Dir = r.repoDir
		cmd.Env = append(os.Environ(), "HOME=/data")
		if o, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("sops encrypt: %v: %s", err, o)
		}
	} else if err := os.WriteFile(out, content, 0o644); err != nil {
		return err
	}
	_, err := r.git("add", gitPath)
	return err
}

// ---------- three-way state ----------

// same treats an absent file and an empty one as different.
func same(a, b []byte) bool {
	return (a == nil) == (b == nil) && bytes.Equal(a, b)
}

func (r *Reconciler) baselineFor(rel string) string {
	if sha, ok := r.state.Baselines[rel]; ok {
		return sha
	}
	return r.state.AppliedSHA
}

// token fingerprints the three versions a decision was made on.
func token(vs ...[]byte) string {
	h := sha256.New()
	for _, v := range vs {
		if v == nil {
			h.Write([]byte{0})
			continue
		}
		fmt.Fprintf(h, "\x01%d:", len(v))
		h.Write(v)
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

func (r *Reconciler) fileState(rel, remote string) FileState {
	baseSHA := r.baselineFor(rel)
	b, l, t := r.atRef(baseSHA, rel), r.live(rel), r.atRef(remote, rel)
	fs := FileState{Rel: rel, BaseSHA: baseSHA, Base: b, Live: l, Target: t}
	if same(l, t) {
		return fs
	}
	lc, tc := !same(l, b), !same(t, b)
	switch {
	case lc && tc:
		fs.Kind = kindBoth
	case lc:
		fs.Kind = kindLive
	default:
		fs.Kind = kindTarget
	}
	fs.Rows = diff3.Align(diff3.Lines(b), diff3.Lines(l), diff3.Lines(t))
	fs.Token = token(b, l, t)
	return fs
}

// computeLocked rebuilds the per-file state against remote and advances
// baselines: a file whose live content matches target is in sync at
// remote, and once no file is behind remote AppliedSHA moves there too.
func (r *Reconciler) computeLocked(remote string) error {
	seen := map[string]bool{}
	var rels []string
	for _, ref := range []string{r.state.AppliedSHA, remote} {
		rs, err := r.managedRels(ref)
		if err != nil {
			return err
		}
		for _, rel := range rs {
			if !seen[rel] {
				seen[rel] = true
				rels = append(rels, rel)
			}
		}
	}
	for rel := range r.state.Baselines {
		if !seen[rel] {
			seen[rel] = true
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)

	var files []FileState
	dirty, behind := false, false
	for _, rel := range rels {
		fs := r.fileState(rel, remote)
		if fs.Kind == "" {
			if fs.BaseSHA != remote && !same(fs.Base, fs.Target) {
				if r.state.Baselines == nil {
					r.state.Baselines = map[string]string{}
				}
				r.state.Baselines[rel] = remote
				dirty = true
			}
			continue
		}
		if fs.Kind != kindLive {
			behind = true
		}
		files = append(files, fs)
	}
	if !behind && (r.state.AppliedSHA != remote || len(r.state.Baselines) > 0) {
		// Nothing user-visible would change by moving the baseline (e.g. a
		// sops re-encryption to the same plaintext, or commits live already
		// matches): fast-forward without touching HA.
		log.Printf("fast-forwarded baseline to %.9s", remote)
		r.state.AppliedSHA = remote
		r.state.Baselines = nil
		dirty = true
	}
	if dirty {
		if err := r.saveState(); err != nil {
			return err
		}
	}
	r.status.Files = files
	r.status.TargetSHA = remote
	return nil
}

// Tick fetches upstream and recomputes. It never writes to live HA —
// upstream commits wait as reviewable target changes until an explicit
// apply.
func (r *Reconciler) Tick() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	remote, err := r.fetchRemote()
	if err != nil {
		return err
	}
	if r.state.AppliedSHA == "" {
		// First run: baseline only, write nothing. Differences appear for
		// the human to converge deliberately.
		r.state.AppliedSHA = remote
		if err := r.saveState(); err != nil {
			return err
		}
		log.Printf("baselined at %.9s without applying (first run)", remote)
	}
	if err := r.computeLocked(remote); err != nil {
		return err
	}
	r.status.Error = ""
	r.finishLocked()
	return nil
}

// finishLocked publishes the recomputed state.
func (r *Reconciler) finishLocked() {
	r.publish()
	r.notifyDrift()
	r.status.LastSync = time.Now().Format("2006-01-02 15:04:05")
}

func (r *Reconciler) RefreshNow() error { return r.Tick() }

// ---------- writes ----------

// Picks stages one file's buffer: row index → the column it takes.
type Picks struct {
	Rel   string
	Token string
	Rows  map[int]diff3.Col
}

// resolve rebuilds a file's current state and checks it is the one the
// picks were made on. Only files the panel was shown can be written.
func (r *Reconciler) resolve(p Picks, remote string, allowed ...diff3.Col) (FileState, error) {
	known := false
	for _, f := range r.status.Files {
		if f.Rel == p.Rel {
			known = true
		}
	}
	if !known {
		return FileState{}, fmt.Errorf("%s: %w", p.Rel, ErrStale)
	}
	fs := r.fileState(p.Rel, remote)
	if fs.Token != p.Token {
		return FileState{}, fmt.Errorf("%s: %w", p.Rel, ErrStale)
	}
	for i, c := range p.Rows {
		ok := false
		for _, a := range allowed {
			ok = ok || c == a
		}
		if !ok || i < 0 || i >= len(fs.Rows) {
			return FileState{}, fmt.Errorf("%s: invalid pick for row %d", p.Rel, i)
		}
	}
	return fs, nil
}

// ApplyLive writes the staged live buffer: each file's live content with
// the picked rows taken from last-applied or target.
func (r *Reconciler) ApplyLive(picks []Picks) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	remote := r.status.TargetSHA
	want := map[string][]byte{}
	for _, p := range picks {
		fs, err := r.resolve(p, remote, diff3.Base, diff3.Target)
		if err != nil {
			return err
		}
		want[p.Rel] = diff3.Compose(fs.Rows, diff3.Live, p.Rows)
	}
	applyErr := r.applyLocked(want)
	if err := r.computeLocked(remote); err != nil {
		return err
	}
	r.finishLocked()
	return applyErr
}

// AutoApply applies target to every file only git changed — the
// auto_apply policy. Files changed live are left for a human.
func (r *Reconciler) AutoApply() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	remote := r.status.TargetSHA
	want := map[string][]byte{}
	for _, f := range r.status.Files {
		if fs := r.fileState(f.Rel, remote); fs.Kind == kindTarget {
			want[f.Rel] = fs.Target
		}
	}
	if len(want) == 0 {
		return nil
	}
	applyErr := r.applyLocked(want)
	if err := r.computeLocked(remote); err != nil {
		return err
	}
	r.finishLocked()
	return applyErr
}

// applyLocked writes desired live contents: backed up, config-checked
// (rolled back on failure), then activated via targeted reloads.
func (r *Reconciler) applyLocked(want map[string][]byte) error {
	backupDir := filepath.Join(r.backupsDir, time.Now().Format("20060102-150405"))
	saved := map[string][]byte{}
	var written []string
	rollback := func() {
		for _, rel := range written {
			if err := r.writeLive(rel, saved[rel]); err != nil {
				log.Printf("rollback %s: %v", rel, err)
			}
		}
	}
	rels := make([]string, 0, len(want))
	for rel := range want {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		liveB := r.live(rel)
		if same(liveB, want[rel]) {
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
		saved[rel] = liveB
		if err := r.writeLive(rel, want[rel]); err != nil {
			rollback()
			return fmt.Errorf("apply %s: %w", rel, err)
		}
		written = append(written, rel)
	}
	if len(written) == 0 {
		return nil
	}
	ok, err := r.ha.CoreCheck()
	if err != nil {
		rollback()
		return err
	}
	if !ok {
		rollback()
		r.ha.Notify("GitOps apply rolled back", fmt.Sprintf(
			"%s failed core config check; live files restored.", strings.Join(written, ", ")))
		return ErrRolledBack
	}
	r.activate(written)
	log.Printf("applied to live: %v", written)
	return nil
}

// CommitTarget commits the staged target buffer — each file's target
// content with the picked rows taken from live — and pushes it.
func (r *Reconciler) CommitTarget(picks []Picks, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	remote, err := r.fetchRemote()
	if err != nil {
		return err
	}
	if remote != r.status.TargetSHA {
		return fmt.Errorf("target moved to %.9s: %w", remote, ErrStale)
	}
	// Start from a clean worktree at the remote head so leftovers from a
	// previously failed commit (staged files, unpushed commits) can't
	// poison this attempt.
	if _, err := r.git("reset", "--hard", "origin/"+r.opts.Branch); err != nil {
		return err
	}
	var rels []string
	for _, p := range picks {
		fs, err := r.resolve(p, remote, diff3.Live)
		if err != nil {
			return err
		}
		if err := r.writeRepo(p.Rel, diff3.Compose(fs.Rows, diff3.Target, p.Rows)); err != nil {
			return err
		}
		rels = append(rels, p.Rel)
	}
	head := remote
	if _, err := r.git("diff", "--cached", "--quiet"); err == nil {
		// Nothing staged — the repo already matches (e.g. a retried
		// commit that had in fact been pushed). Recompute and move on.
		log.Printf("commit: nothing to commit for %v", rels)
	} else {
		if message == "" {
			message = "promote: " + strings.Join(rels, ", ")
		}
		if _, err := r.git("commit", "-m", message); err != nil {
			return err
		}
		if _, err := r.git("push", "origin", "HEAD:"+r.opts.Branch); err != nil {
			_, _ = r.git("reset", "--hard", "origin/"+r.opts.Branch)
			return fmt.Errorf("push failed, worktree reset — refresh and retry: %v", err)
		}
		out, err := r.git("rev-parse", "HEAD")
		if err != nil {
			return err
		}
		head = strings.TrimSpace(out)
	}
	if err := r.computeLocked(head); err != nil {
		return err
	}
	r.finishLocked()
	return nil
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

// ---------- reporting ----------

func (r *Reconciler) byKind(kind string) []string {
	var out []string
	for _, f := range r.status.Files {
		if f.Kind == kind {
			out = append(out, f.Rel)
		}
	}
	return out
}

func (r *Reconciler) publish() {
	sha := r.state.AppliedSHA
	if len(sha) > 9 {
		sha = sha[:9]
	}
	r.ha.SetState("sensor.ha_gitops_drift", fmt.Sprint(len(r.status.Files)), map[string]any{
		"friendly_name":    "GitOps drift",
		"icon":             "mdi:source-branch-sync",
		"drift":            r.byKind(kindLive),
		"incoming":         r.byKind(kindTarget),
		"conflicts":        r.byKind(kindBoth),
		"applied_sha":      sha,
		"restart_required": r.status.RestartRequired,
	})
}

const driftNotifyTitle = "GitOps: drift detected"

var kindReason = map[string]string{
	kindLive:   "changed in HA",
	kindTarget: "changed in git",
	kindBoth:   "changed in both git and HA",
}

// notifyDrift maintains one persistent notification listing every file
// that needs a human decision. It only touches HA when the set changes:
// new files push through notify_service too, a shrinking set refreshes
// the notification silently, and an empty set dismisses it. In-memory
// tracking means an add-on restart re-notifies once, which doubles as a
// reminder. Git-only changes are excluded under auto_apply — those files
// resolve on their own moments later and would only generate push noise.
func (r *Reconciler) notifyDrift() {
	if !r.opts.NotifyDrift {
		return
	}
	current := map[string]string{}
	for _, f := range r.status.Files {
		if f.Kind == kindTarget && r.opts.AutoApply {
			continue
		}
		current[f.Rel] = kindReason[f.Kind]
	}

	if len(current) == 0 {
		if len(r.notified) > 0 {
			r.ha.Dismiss(driftNotifyTitle)
			r.notified = nil
		}
		return
	}
	hasNew := false
	for rel := range current {
		if !r.notified[rel] {
			hasNew = true
		}
	}
	if !hasNew && len(current) == len(r.notified) {
		return // same set as already notified
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) differ between git and HA:\n", len(current))
	for _, rel := range sortedKeys(current) {
		fmt.Fprintf(&b, "- %s — %s\n", rel, current[rel])
	}
	b.WriteString("\nOpen the GitOps panel to apply or commit.")
	if hasNew {
		r.ha.Notify(driftNotifyTitle, b.String())
	} else {
		r.ha.Persist(driftNotifyTitle, b.String())
	}
	r.notified = map[string]bool{}
	for rel := range current {
		r.notified[rel] = true
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
