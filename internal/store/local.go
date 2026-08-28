package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Local is the plan store on a filesystem, using the same key layout as the bucket in
// DESIGN.md 5:
//
//	<Root>/plans/<owner>/<repo>/<workspace>/<base_sha>/<head_sha>/
//	    tfplan       the opaque binary — the thing that gets applied
//	    plan.json    terraform show -json
//	    plan.txt     terraform show
//	    meta.json    provenance, digest-checked
//	    state.json   planned | applying | applied | apply_failed
//	    apply-1.log
//
// It backs cmd/tfog-plan and cmd/tfog-apply. Keeping the bucket's layout means the local store is
// a rehearsal for GCS rather than a parallel invention: the same (base_sha, head_sha) pair is the
// key, so "the reviewed plan" and "the applied plan" are one object rather than two things that
// ought to agree.
//
// Two properties from the design survive here, and one does not.
//
// Survives — write-once (5.2). Artifacts are created with O_EXCL at mode 0600. A second plan of
// the same pair cannot silently replace the bytes a reviewer read; Purge deletes the whole key
// first, visibly, which is what --force does.
//
// Survives — tamper evidence. meta.json carries `digest`, over Meta.Digest()'s canonical
// encoding, and PlanSHA256 over the tfplan bytes. ReadMeta verifies the first and refuses an
// unverifiable record; the apply command checks the second.
//
// Does not survive — the KMS signature (5.3). `digest` catches a careless edit or a half-written
// file. It stops nobody who can write to the store, because the store's owner computes it. On one
// machine under one operator the filesystem is the boundary. Do not move this directory to a
// shared host and call it signed.
type Local struct {
	Root string
}

// RunState values are the same lifecycle as the coordination bucket's run index, minus the states
// that only exist because there are two workers racing.
const (
	ArtifactState = "state.json"
)

// LocalRun is the local stand-in for Run. It answers exactly one question — has this plan already
// been applied — so it is a plain rewrite with no compare-and-set. One operator, one process:
// there is no race to lose.
type LocalRun struct {
	State          RunState  `json:"state"`
	PR             int       `json:"pr,omitempty"`
	Attempt        int       `json:"attempt,omitempty"`
	MergeCommitSHA string    `json:"merge_commit_sha,omitempty"`
	Note           string    `json:"note,omitempty"`
	Error          string    `json:"error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ErrExists means an artifact is already present. Write-once is enforced by the filesystem, so
// this is O_EXCL failing rather than a check-then-write.
var ErrExists = errors.New("store: artifact already exists (the store is write-once)")

// ErrNoPlan means there is no complete plan at this key.
var ErrNoPlan = errors.New("store: no complete plan at this key")

// ErrBadDigest means meta.json does not match its own recorded digest.
var ErrBadDigest = errors.New("store: meta.json digest mismatch")

func (l *Local) KeyDir(k PlanKey) string {
	return filepath.Join(l.Root, "plans", filepath.FromSlash(k.Prefix()))
}

func (l *Local) Path(k PlanKey, artifact string) string {
	return filepath.Join(l.KeyDir(k), artifact)
}

// Complete reports whether a whole plan is present. meta.json is written last, so its presence is
// the completion marker — the same ordering the bucket relies on.
func (l *Local) Complete(k PlanKey) bool {
	_, err := os.Stat(l.Path(k, ArtifactMeta))
	return err == nil
}

func (l *Local) mkdir(k PlanKey) error {
	dir := l.KeyDir(k)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 0700 all the way down. A plan file embeds a snapshot of prior state, so every secret in
	// state is in this directory (DESIGN 5.1) — it is exactly as sensitive as the state bucket.
	for _, p := range []string{l.Root, filepath.Join(l.Root, "plans"), dir} {
		_ = os.Chmod(p, 0o700)
	}
	return nil
}

// Put writes one artifact, failing if it already exists.
func (l *Local) Put(k PlanKey, artifact string, r io.Reader) error {
	if err := l.mkdir(k); err != nil {
		return err
	}
	path := l.Path(k, artifact)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Sync()
}

// PutFile copies a file into the store as one artifact.
func (l *Local) PutFile(k PlanKey, artifact, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return l.Put(k, artifact, f)
}

// PutMeta computes the digest and writes meta.json. Last write of the sequence.
func (l *Local) PutMeta(k PlanKey, m Meta) (Meta, error) {
	digest, err := m.DigestHex()
	if err != nil {
		return Meta{}, err
	}
	rec := metaRecord{Meta: m, Digest: digest}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if err := l.Put(k, ArtifactMeta, strings.NewReader(string(body)+"\n")); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// metaRecord is Meta plus the digest over it. The digest is not part of Meta because it is not
// part of what is digested — including it would make the computation self-referential.
type metaRecord struct {
	Meta
	Digest string `json:"digest"`
}

// ReadMeta reads and verifies meta.json.
//
// It MUST NOT return an unverified Meta under any circumstance. A record whose digest does not
// match, or which carries no digest at all, is a hard failure — never something to work around by
// re-planning, because re-planning is exactly how an unreviewed change gets applied.
func (l *Local) ReadMeta(k PlanKey) (Meta, error) {
	path := l.Path(k, ArtifactMeta)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, fmt.Errorf("%w: %s", ErrNoPlan, l.KeyDir(k))
	}
	if err != nil {
		return Meta{}, err
	}

	var rec metaRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return Meta{}, fmt.Errorf("%s: %w", path, err)
	}
	if rec.Digest == "" {
		return Meta{}, fmt.Errorf("%w: %s has no digest; refusing to use it", ErrBadDigest, path)
	}
	want, err := rec.Meta.DigestHex()
	if err != nil {
		return Meta{}, err
	}
	if want != rec.Digest {
		return Meta{}, fmt.Errorf("%w: %s records %s, computed %s — the record was edited after it was written",
			ErrBadDigest, path, shortHex(rec.Digest), shortHex(want))
	}
	return rec.Meta, nil
}

// Get opens an artifact for reading.
func (l *Local) Get(k PlanKey, artifact string) (io.ReadCloser, error) {
	return os.Open(l.Path(k, artifact))
}

// ReadArtifact reads one artifact whole.
func (l *Local) ReadArtifact(k PlanKey, artifact string) ([]byte, error) {
	return os.ReadFile(l.Path(k, artifact))
}

// SHA256 digests one stored artifact, for checking tfplan against Meta.PlanSHA256.
func (l *Local) SHA256(k PlanKey, artifact string) (string, error) {
	f, err := os.Open(l.Path(k, artifact))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadRun reads state.json, returning a zero LocalRun when there is none.
func (l *Local) ReadRun(k PlanKey) LocalRun {
	body, err := os.ReadFile(l.Path(k, ArtifactState))
	if err != nil {
		return LocalRun{}
	}
	var r LocalRun
	if err := json.Unmarshal(body, &r); err != nil {
		return LocalRun{}
	}
	return r
}

// WriteRun replaces state.json atomically.
func (l *Local) WriteRun(k PlanKey, r LocalRun) error {
	if err := l.mkdir(k); err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := l.Path(k, ArtifactState)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NextApplyLog returns the path and attempt number for the next apply log.
func (l *Local) NextApplyLog(k PlanKey) (string, int, error) {
	if err := l.mkdir(k); err != nil {
		return "", 0, err
	}
	existing, _ := filepath.Glob(filepath.Join(l.KeyDir(k), "apply-*.log"))
	attempt := len(existing) + 1
	return l.Path(k, fmt.Sprintf("apply-%d.log", attempt)), attempt, nil
}

// Purge removes a key entirely. The only way to replace a stored plan, and deliberately loud
// about it — see --force in cmd/tfog-plan.
func (l *Local) Purge(k PlanKey) error {
	return os.RemoveAll(l.KeyDir(k))
}

// Find returns every complete plan for a workspace, optionally pinned to one head SHA.
//
// This is how the apply command locates the reviewed plan: it knows the merged PR's head SHA, and
// the head SHA is half the key. base_sha is globbed rather than derived, because by apply time
// the base branch tip has moved past what was planned — so it cannot be recomputed, only
// remembered.
func (l *Local) Find(owner, repo, workspace, headSHA string) ([]PlanKey, error) {
	base := filepath.Join(l.Root, "plans", owner, repo, workspace)
	baseDirs, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var keys []PlanKey
	for _, bd := range baseDirs {
		if !bd.IsDir() {
			continue
		}
		heads, err := os.ReadDir(filepath.Join(base, bd.Name()))
		if err != nil {
			continue
		}
		for _, hd := range heads {
			if !hd.IsDir() || (headSHA != "" && hd.Name() != headSHA) {
				continue
			}
			k := PlanKey{Owner: owner, Repo: repo, Workspace: workspace, BaseSHA: bd.Name(), HeadSHA: hd.Name()}
			if l.Complete(k) {
				keys = append(keys, k)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].BaseSHA != keys[j].BaseSHA {
			return keys[i].BaseSHA < keys[j].BaseSHA
		}
		return keys[i].HeadSHA < keys[j].HeadSHA
	})
	return keys, nil
}

func shortHex(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
