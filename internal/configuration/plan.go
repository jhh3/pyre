package configuration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// fileOp is one config-home mutation inside an apply transaction.
type fileOp struct {
	Kind    string            `json:"kind"` // "dir_put" | "dir_delete" | "json_write"
	Path    string            `json:"path"`
	Display string            `json:"display"`
	Files   map[string][]byte `json:"files,omitempty"`   // dir_put: full content including marker
	Content []byte            `json:"content,omitempty"` // json_write: full new file content
	Base    contracts.Digest  `json:"base"`              // live digest at plan time ("" = absent)
}

func (o fileOp) contentDigest() string {
	switch o.Kind {
	case "dir_put":
		return string(extensions.DigestFileMap(o.Files))
	case "json_write":
		return string(extensions.DigestBytes(o.Content))
	}
	return ""
}

// planRecord persists a reviewed plan with everything apply needs.
type planRecord struct {
	Plan         contracts.ChangePlan      `json:"plan"`
	Gen          genRecord                 `json:"gen"`
	OverlayFiles map[string][]byte         `json:"overlay_files"`
	HomeFiles    map[int]map[string][]byte `json:"home_files,omitempty"` // targetRecord.Index -> dir content (rollback copy)
	Ops          []fileOp                  `json:"ops"`
	Noop         bool                      `json:"noop"`
	ReuseID      string                    `json:"reuse_id,omitempty"`
	Applied      bool                      `json:"applied"`
}

// Plan produces a reviewable ChangePlan. Nothing is written except the plan
// record under the Pyre state dir.
func (m *Manager) Plan(ctx context.Context, p contracts.EnvPlanParams) (contracts.ChangePlan, error) {
	sc, err := parseScope(p)
	if err != nil {
		return contracts.ChangePlan{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	res, err := m.resolve(ctx, sc)
	if err != nil {
		return contracts.ChangePlan{}, err
	}
	prev := m.currentRecordFor(sc.Scope)
	planID := contracts.NewID("plan")
	rec := &planRecord{OverlayFiles: res.overlayFiles, HomeFiles: map[int]map[string][]byte{}}
	plan := contracts.ChangePlan{PlanID: planID, NodeID: m.opt.NodeID, Scope: sc.Scope, Changes: []contracts.PlanChange{}, Conflicts: []contracts.DriftItem{}, RestartRequired: []string{}}

	genID := contracts.NewID(contracts.PrefixEnvironment)
	desired := make([]desiredTarget, len(res.targets))
	copy(desired, res.targets)
	ops, changes, conflicts, records := m.computeOps(planID, genID, desired, prev)
	plan.Conflicts = append(plan.Conflicts, res.conflicts...)
	plan.Conflicts = append(plan.Conflicts, conflicts...)

	sameAsCurrent := prev != nil && prev.Generation.Digest == res.gen.Digest && len(ops) == 0
	if sameAsCurrent {
		rec.Noop = true
		rec.ReuseID = prev.Generation.GenerationID
	} else {
		// Overlay files are new immutable per-generation files.
		for _, rel := range sortedFileKeys(res.overlayFiles) {
			plan.Changes = append(plan.Changes, contracts.PlanChange{Path: "~generation/" + rel, Action: "create",
				Diff: summarizeOverlay(rel, res.overlayFiles[rel])})
		}
		if len(res.overlay.CodexArgs) > 0 {
			plan.Changes = append(plan.Changes, contracts.PlanChange{Path: "codex:-c", Action: "update", Diff: codexArgsDiff(prev, res.overlay.CodexArgs)})
		}
		plan.Changes = append(plan.Changes, registrationChanges(prev, res)...)
		plan.Changes = append(plan.Changes, changes...)
		plan.RestartRequired = m.restartRequired(ctx, sc, res.gen.Digest)
	}

	gen := res.gen
	gen.GenerationID = genID
	gen.CreatedAt = m.opt.Clock.Now()
	if prev != nil {
		gen.PreviousID = prev.Generation.GenerationID
		markRestart(&gen, &prev.Generation, m.opt.HotReloadKeys)
	}
	rec.Gen = genRecord{Generation: gen, Scope: sc.Scope, Source: "apply", Overlay: res.overlay, Home: records, Baseline: res.baseline,
		Profile: res.snapshotFor(sc)}
	for i, t := range desired {
		if t.Kind == "dir" {
			rec.HomeFiles[records[i].Index] = t.Files
		}
	}
	rec.Ops = ops
	plan.ResultDigest = planDigest(sc.Scope, res.gen.Digest, ops, rec.Noop)
	rec.Plan = plan
	if err := writeFileAtomic(m.planPath(planID), prettyJSON(rec), 0o600); err != nil {
		return contracts.ChangePlan{}, err
	}
	return plan, nil
}

// Preview resolves a scope without planning or applying (env.resolve).
func (m *Manager) Preview(ctx context.Context, p contracts.EnvPlanParams) (contracts.EnvironmentGeneration, error) {
	sc, err := parseScope(p)
	if err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	res, err := m.resolve(ctx, sc)
	if err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	g := res.gen
	g.CreatedAt = m.opt.Clock.Now()
	if prev := m.currentRecordFor(sc.Scope); prev != nil {
		g.PreviousID = prev.Generation.GenerationID
		markRestart(&g, &prev.Generation, m.opt.HotReloadKeys)
	}
	return g, nil
}

// Apply applies a reviewed plan. The caller must pass the plan's
// ResultDigest; config-home files must be unchanged since the plan. File
// changes are atomic per file and the whole set is rolled back if any step
// fails.
func (m *Manager) Apply(ctx context.Context, p contracts.EnvApplyParams) (contracts.EnvironmentGeneration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !contracts.ValidID(p.PlanID, "plan") {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrInvalid, "invalid plan id %q", p.PlanID)
	}
	b, err := os.ReadFile(m.planPath(p.PlanID))
	if os.IsNotExist(err) {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrNotFound, "plan %s not found; run env.plan first", p.PlanID)
	}
	if err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	var rec planRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	if p.ResultDigest != rec.Plan.ResultDigest {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrConflict, "result_digest does not match plan %s; review the current plan before applying", p.PlanID).
			WithDetail("expected", rec.Plan.ResultDigest)
	}
	if rec.Applied {
		id := rec.Gen.Generation.GenerationID
		if rec.Noop {
			id = rec.ReuseID
		}
		return m.Generation(ctx, id) // idempotent retry
	}
	var blocking []contracts.DriftItem
	for _, c := range rec.Plan.Conflicts {
		if c.Kind == "conflict" {
			blocking = append(blocking, c)
		}
	}
	if len(blocking) > 0 {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrConflict, "plan %s has %d unresolved conflict(s); user configuration is not overwritten", p.PlanID, len(blocking)).
			WithDetail("conflicts", blocking)
	}
	if prev := m.currentRecordFor(rec.Plan.Scope); prevID(prev) != rec.Gen.Generation.PreviousID && !rec.Noop {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrConflict, "scope %s changed since plan %s (current generation %s); re-run env.plan", rec.Plan.Scope, p.PlanID, prevID(prev))
	}
	if rec.Noop {
		if err := m.promoteProfile(&rec.Gen); err != nil {
			return contracts.EnvironmentGeneration{}, err
		}
		rec.Applied = true
		_ = writeFileAtomic(m.planPath(p.PlanID), prettyJSON(rec), 0o600)
		return m.Generation(ctx, rec.ReuseID)
	}
	for _, op := range rec.Ops {
		d, _, err := liveDigest(op.Path)
		if err != nil {
			return contracts.EnvironmentGeneration{}, err
		}
		if d != op.Base {
			return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrConflict, "%s changed since plan %s; re-run env.plan (nothing was modified)", op.Display, p.PlanID)
		}
	}
	gen := &rec.Gen
	if err := m.writeGeneration(gen, rec.OverlayFiles, rec.HomeFiles); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	if err := m.execOps(p.PlanID, rec.Ops); err != nil {
		os.RemoveAll(m.genDir(gen.Generation.GenerationID))
		return contracts.EnvironmentGeneration{}, err
	}
	now := m.opt.Clock.Now()
	gen.Applied, gen.AppliedAt = true, &now
	if err := m.saveRecord(gen); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	_ = m.indexGeneration(gen.Generation.Digest, gen.Generation.GenerationID)
	if err := m.setCurrent(gen.Scope, gen.Generation.GenerationID); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	if err := m.promoteProfile(gen); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	rec.Applied = true
	_ = writeFileAtomic(m.planPath(p.PlanID), prettyJSON(rec), 0o600)
	m.reportApplied(ctx, gen, map[string]any{"plan_id": p.PlanID, "scope": gen.Scope, "restart_required": rec.Plan.RestartRequired})
	return gen.Generation, nil
}

// Rollback re-materializes a previous generation for its scope and makes it
// current. Running runs keep the generation they were launched with.
func (m *Manager) Rollback(ctx context.Context, p contracts.EnvRollbackParams) (contracts.EnvironmentGeneration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, err := m.loadRecord(p.GenerationID)
	if err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	if target.Generation.NodeID != m.opt.NodeID {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrInvalid, "generation %s belongs to node %s", p.GenerationID, target.Generation.NodeID)
	}
	for rel, d := range target.Overlay.Files {
		got, ok, _ := liveDigest(filepath.Join(m.genDir(target.Generation.GenerationID), filepath.FromSlash(rel)))
		if !ok || got != d {
			return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrConflict, "generation %s overlay file %s is missing or modified; cannot roll back to it", p.GenerationID, rel)
		}
	}
	prev := m.currentRecordFor(target.Scope)
	if prev != nil && prev.Generation.GenerationID == target.Generation.GenerationID {
		return target.Generation, nil
	}
	desired := make([]desiredTarget, 0, len(target.Home))
	for _, t := range target.Home {
		dt := desiredTarget{Kind: t.Kind, Path: t.Path, Display: t.Display, Name: t.Name, Keys: t.Keys, Layer: contracts.LayerManagedGlobal}
		if t.Kind == "dir" {
			files, err := readTree(filepath.Join(m.genDir(target.Generation.GenerationID), "home", fmt.Sprint(t.Index)))
			if err != nil {
				return contracts.EnvironmentGeneration{}, fmt.Errorf("rollback: stored content for %s: %w", t.Display, err)
			}
			dt.Files = files
		}
		desired = append(desired, dt)
	}
	planID := contracts.NewID("plan")
	ops, _, conflicts, _ := m.computeOps(planID, target.Generation.GenerationID, desired, prev)
	var blocking []contracts.DriftItem
	for _, c := range conflicts {
		if c.Kind == "conflict" {
			blocking = append(blocking, c)
		}
	}
	if len(blocking) > 0 {
		return contracts.EnvironmentGeneration{}, contracts.Errorf(contracts.ErrConflict, "rollback to %s blocked by %d local change(s) to managed items; resolve them first (nothing was modified)", p.GenerationID, len(blocking)).
			WithDetail("conflicts", blocking)
	}
	if err := m.execOps(planID, ops); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	if !target.Applied {
		now := m.opt.Clock.Now()
		target.Applied, target.AppliedAt = true, &now
		if err := m.saveRecord(target); err != nil {
			return contracts.EnvironmentGeneration{}, err
		}
	}
	if err := m.setCurrent(target.Scope, target.Generation.GenerationID); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	if err := m.promoteProfile(target); err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	m.reportApplied(ctx, target, map[string]any{"rollback_from": prevID(prev), "scope": target.Scope})
	return target.Generation, nil
}

// promoteProfile makes a generation's reviewed profile the active one: it is
// written into the profile store that launches resolve, and a pending desired
// profile it satisfies is cleared. Generations without a recorded profile
// (older records, session scopes) leave the store alone.
func (m *Manager) promoteProfile(rec *genRecord) error {
	if rec.Profile == nil {
		return nil
	}
	projectID := ""
	switch {
	case rec.Scope == "global":
	case strings.HasPrefix(rec.Scope, "project:"):
		projectID = strings.TrimPrefix(rec.Scope, "project:")
	default:
		return nil
	}
	name := orDefault(rec.Profile.Name, "default")
	if rec.Profile.Present && rec.Profile.Profile != nil {
		p := *rec.Profile.Profile
		p.Name = name
		if err := m.profiles.Save(projectID, p); err != nil {
			return err
		}
	} else if err := os.Remove(m.profiles.path(projectID, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if d, err := m.desired.Load(projectID, name); err == nil && d != nil && profileDigest(d) == profileDigest(rec.Profile.Profile) {
		_ = os.Remove(m.desired.path(projectID, name))
	}
	return nil
}

func prevID(r *genRecord) string {
	if r == nil {
		return ""
	}
	return r.Generation.GenerationID
}

func (m *Manager) currentRecordFor(scope string) *genRecord {
	id := m.currentID(scope)
	if id == "" {
		return nil
	}
	r, err := m.loadRecord(id)
	if err != nil {
		return nil
	}
	return r
}

func (m *Manager) reportApplied(ctx context.Context, gen *genRecord, extra map[string]any) {
	m.report(ctx, contracts.ReportEnvironment, gen.Generation)
	payload := map[string]any{"generation_id": gen.Generation.GenerationID, "digest": gen.Generation.Digest}
	for k, v := range extra {
		payload[k] = v
	}
	pid := ""
	if gen.Generation.ProjectID != nil {
		pid = *gen.Generation.ProjectID
	}
	m.report(ctx, contracts.ReportEvent, m.event(contracts.EvEnvironmentApplied, contracts.ObjectRef{Kind: contracts.KindEnvironment, ID: gen.Generation.GenerationID}, pid, payload))
}

// writeGeneration materializes the immutable generation directory via a
// temp directory and rename.
func (m *Manager) writeGeneration(gen *genRecord, overlay map[string][]byte, home map[int]map[string][]byte) error {
	dir := m.genDir(gen.Generation.GenerationID)
	if _, err := os.Stat(dir); err == nil {
		return contracts.Errorf(contracts.ErrConflict, "generation %s already exists", gen.Generation.GenerationID)
	}
	tmp := filepath.Join(filepath.Dir(dir), ".tmp-"+gen.Generation.GenerationID+"-"+randSuffix())
	if err := writeTree(tmp, overlay, 0o600); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	for idx, files := range home {
		if err := writeTree(filepath.Join(tmp, "home", fmt.Sprint(idx)), files, 0o600); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "record.json"), prettyJSON(gen), 0o600); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	syncDir(filepath.Dir(dir))
	return nil
}

// computeOps diffs desired config-home targets against live files and the
// previous applied generation's ownership records.
func (m *Manager) computeOps(planID, genID string, desired []desiredTarget, prev *genRecord) ([]fileOp, []contracts.PlanChange, []contracts.DriftItem, []targetRecord) {
	var ops []fileOp
	var changes []contracts.PlanChange
	var conflicts []contracts.DriftItem
	var records []targetRecord
	prevByPath := map[string]targetRecord{}
	if prev != nil {
		for _, t := range prev.Home {
			prevByPath[t.Kind+":"+t.Path] = t
		}
	}
	backupFor := func(path string) string {
		return m.display(filepath.Join(m.backupDir(planID), fmt.Sprintf("%d-%s", len(ops), filepath.Base(path))))
	}
	want := map[string]bool{}
	for i, t := range desired {
		want[t.Kind+":"+t.Path] = true
		rec := targetRecord{Kind: t.Kind, Path: t.Path, Display: t.Display, Name: t.Name, Index: i, Digest: t.digest(), Keys: t.Keys}
		records = append(records, rec)
		switch t.Kind {
		case "dir":
			live, exists, err := liveDigest(t.Path)
			if err != nil {
				conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Kind: "conflict", Detail: "cannot read: " + err.Error()})
				continue
			}
			marker, owned := extensions.ReadMarker(t.Path)
			desiredDigest := extensions.DigestFileMap(t.Files)
			files := map[string][]byte{}
			for k, v := range t.Files {
				files[k] = v
			}
			files[extensions.MarkerFile] = prettyJSON(extensions.Marker{Owner: "pyre", Name: t.Name, Kind: extensions.KindSkill, Layer: t.Layer, GenerationID: genID, Digest: desiredDigest, Version: t.Version})
			switch {
			case !exists:
				ops = append(ops, fileOp{Kind: "dir_put", Path: t.Path, Display: t.Display, Files: files})
				changes = append(changes, contracts.PlanChange{Path: t.Display, Action: "create", Diff: treeSummaryDiff(nil, t.Files)})
			case !owned:
				if live == desiredDigest {
					continue // identical user-owned copy: referenced, never taken over
				}
				conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Key: "skill:" + t.Name, Kind: "conflict",
					Detail: "a user-owned directory with this name exists (no Pyre ownership marker); it is preserved. Rename the managed skill or remove the user copy."})
			case live == desiredDigest:
				// already materialized
			case live != marker.Digest:
				conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Key: "skill:" + t.Name, Kind: "conflict", Expected: string(marker.Digest), Actual: string(live),
					Detail: "managed directory was modified locally; Pyre will not overwrite local edits"})
			default:
				oldFiles, _ := readTree(t.Path)
				delete(oldFiles, extensions.MarkerFile)
				ops = append(ops, fileOp{Kind: "dir_put", Path: t.Path, Display: t.Display, Files: files, Base: live})
				changes = append(changes, contracts.PlanChange{Path: t.Display, Action: "update", Diff: treeSummaryDiff(oldFiles, t.Files), Backup: backupFor(t.Path)})
			}
		case "json_keys":
			op, ch, cs := m.jsonKeysOp(t, prevByPath["json_keys:"+t.Path].Keys, backupFor(t.Path))
			conflicts = append(conflicts, cs...)
			if op != nil {
				ops = append(ops, *op)
				changes = append(changes, ch)
			}
		}
	}
	// Targets managed by the previous generation but no longer desired.
	if prev != nil {
		for _, t := range prev.Home {
			if want[t.Kind+":"+t.Path] {
				continue
			}
			switch t.Kind {
			case "dir":
				live, exists, _ := liveDigest(t.Path)
				if !exists {
					continue
				}
				marker, owned := extensions.ReadMarker(t.Path)
				if !owned || live != marker.Digest {
					conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Key: "skill:" + t.Name, Kind: "conflict",
						Detail: "no longer managed but modified locally (or marker removed); left in place, not deleted"})
					continue
				}
				ops = append(ops, fileOp{Kind: "dir_delete", Path: t.Path, Display: t.Display, Base: live})
				changes = append(changes, contracts.PlanChange{Path: t.Display, Action: "delete", Backup: backupFor(t.Path)})
			case "json_keys":
				op, ch, cs := m.jsonKeysOp(desiredTarget{Kind: "json_keys", Path: t.Path, Display: t.Display, Keys: map[string]any{}}, t.Keys, backupFor(t.Path))
				conflicts = append(conflicts, cs...)
				if op != nil {
					ops = append(ops, *op)
					changes = append(changes, ch)
				}
			}
		}
	}
	return ops, changes, conflicts, records
}

// jsonKeysOp computes the edit of Pyre-owned keys inside a user JSON file.
// prevKeys are the values Pyre last wrote (its ownership record). Keys Pyre
// does not own are never changed; a managed key edited by the user, or a
// user key Pyre would take over with a different value, is a conflict.
func (m *Manager) jsonKeysOp(t desiredTarget, prevKeys map[string]any, backup string) (*fileOp, contracts.PlanChange, []contracts.DriftItem) {
	var conflicts []contracts.DriftItem
	raw, err := os.ReadFile(t.Path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, contracts.PlanChange{}, []contracts.DriftItem{{Path: t.Display, Kind: "conflict", Detail: "cannot read: " + err.Error()}}
	}
	doc, err := parseOrdered(raw)
	if err != nil {
		return nil, contracts.PlanChange{}, []contracts.DriftItem{{Path: t.Display, Kind: "conflict", Detail: "not valid JSON; Pyre will not edit it: " + err.Error()}}
	}
	var diff []string
	changed := false
	for _, k := range sortedMapKeys(t.Keys) {
		v := t.Keys[k]
		segs, _ := SplitKey(k)
		lv, has := doc.getPath(segs)
		var lvp any
		if has {
			lvp = plainValue(lv)
		}
		if has && equalValues(lvp, v) {
			continue
		}
		fullKey := "claude." + k
		if pv, owned := prevKeys[k]; owned {
			if has && !equalValues(lvp, pv) {
				conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Key: k, Kind: "conflict", Expected: redactValue(fullKey, pv), Actual: redactValue(fullKey, lvp),
					Detail: "Pyre-managed key was changed locally; not overwritten"})
				continue
			}
		} else if has {
			conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Key: k, Kind: "conflict", Expected: redactValue(fullKey, v), Actual: redactValue(fullKey, lvp),
				Detail: "user-owned key has a different value; Pyre does not take ownership of user keys (use an overlay instead of persist, or align the value)"})
			continue
		}
		if err := doc.setPath(segs, v); err != nil {
			conflicts = append(conflicts, contracts.DriftItem{Path: t.Display, Key: k, Kind: "conflict", Detail: err.Error()})
			continue
		}
		if has {
			diff = append(diff, "- "+k+": "+redactValue(fullKey, lvp))
		}
		diff = append(diff, "+ "+k+": "+redactValue(fullKey, v))
		changed = true
	}
	for _, k := range sortedMapKeys(prevKeys) {
		if _, still := t.Keys[k]; still {
			continue
		}
		segs, _ := SplitKey(k)
		lv, has := doc.getPath(segs)
		if !has {
			continue
		}
		if !equalValues(plainValue(lv), prevKeys[k]) {
			diff = append(diff, "~ "+k+": modified locally; ownership released, value kept")
			continue
		}
		doc.delPath(segs)
		diff = append(diff, "- "+k+": "+redactValue("claude."+k, prevKeys[k]))
		changed = true
	}
	if !changed {
		return nil, contracts.PlanChange{}, conflicts
	}
	base, _, _ := liveDigest(t.Path)
	op := &fileOp{Kind: "json_write", Path: t.Path, Display: t.Display, Content: doc.marshal(), Base: base}
	action := "update"
	ch := contracts.PlanChange{Path: t.Display, Action: action, Diff: strings.Join(diff, "\n")}
	if exists {
		ch.Backup = backup
	} else {
		ch.Action = "create"
	}
	return op, ch, conflicts
}

// execOps runs ops transactionally: each op backs up what it replaces, and a
// failure undoes every completed op in reverse order.
func (m *Manager) execOps(planID string, ops []fileOp) error {
	type done struct {
		undo   func() error
		commit func()
	}
	var completed []done
	rollback := func(cause error) error {
		var undoErrs []string
		for i := len(completed) - 1; i >= 0; i-- {
			if err := completed[i].undo(); err != nil {
				undoErrs = append(undoErrs, err.Error())
			}
		}
		if len(undoErrs) > 0 {
			return fmt.Errorf("apply failed (%v) and rollback was incomplete: %s; backups are in %s", cause, strings.Join(undoErrs, "; "), m.display(m.backupDir(planID)))
		}
		return fmt.Errorf("apply failed and was rolled back; no managed files changed: %w", cause)
	}
	for i, op := range ops {
		if m.failBefore != nil {
			if err := m.failBefore(op); err != nil {
				return rollback(err)
			}
		}
		backup := filepath.Join(m.backupDir(planID), fmt.Sprintf("%d-%s", i, filepath.Base(op.Path)))
		switch op.Kind {
		case "dir_put", "dir_delete":
			parent := filepath.Dir(op.Path)
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return rollback(err)
			}
			_, exists, _ := liveDigest(op.Path)
			old := filepath.Join(parent, ".pyre-old-"+filepath.Base(op.Path)+"-"+randSuffix())
			if exists {
				if err := backupTree(op.Path, backup); err != nil {
					return rollback(fmt.Errorf("backup %s: %w", op.Display, err))
				}
				if err := os.Rename(op.Path, old); err != nil {
					return rollback(err)
				}
			}
			if op.Kind == "dir_put" {
				stage := filepath.Join(parent, ".pyre-new-"+filepath.Base(op.Path)+"-"+randSuffix())
				if err := writeTree(stage, op.Files, 0o644); err != nil {
					os.RemoveAll(stage)
					if exists {
						_ = os.Rename(old, op.Path)
					}
					return rollback(err)
				}
				if err := os.Rename(stage, op.Path); err != nil {
					os.RemoveAll(stage)
					if exists {
						_ = os.Rename(old, op.Path)
					}
					return rollback(err)
				}
			}
			path, existed, isPut := op.Path, exists, op.Kind == "dir_put"
			completed = append(completed, done{
				undo: func() error {
					if isPut {
						if err := os.RemoveAll(path); err != nil {
							return err
						}
					}
					if existed {
						return os.Rename(old, path)
					}
					return nil
				},
				commit: func() { os.RemoveAll(old) },
			})
		case "json_write":
			oldContent, err := os.ReadFile(op.Path)
			existed := err == nil
			mode := os.FileMode(0o644)
			if existed {
				if st, err := os.Stat(op.Path); err == nil {
					mode = st.Mode().Perm()
				}
				if err := backupFile(op.Path, backup); err != nil {
					return rollback(fmt.Errorf("backup %s: %w", op.Display, err))
				}
			}
			if err := writeFileAtomic(op.Path, op.Content, mode); err != nil {
				return rollback(err)
			}
			path := op.Path
			completed = append(completed, done{
				undo: func() error {
					if existed {
						return writeFileAtomic(path, oldContent, mode)
					}
					return os.Remove(path)
				},
				commit: func() {},
			})
		default:
			return rollback(fmt.Errorf("unknown op %q", op.Kind))
		}
	}
	for _, d := range completed {
		d.commit()
	}
	return nil
}

func planDigest(scope string, genDigest contracts.Digest, ops []fileOp, noop bool) contracts.Digest {
	parts := []string{"scope", scope, "gen", string(genDigest), "noop", fmt.Sprint(noop)}
	for _, o := range ops {
		parts = append(parts, o.Kind, o.Path, string(o.Base), o.contentDigest())
	}
	return extensions.DigestStrings(parts...)
}

// restartRequired lists sessions whose live run uses a generation that
// differs from the candidate within the scope.
func (m *Manager) restartRequired(ctx context.Context, sc scopeSpec, candidate contracts.Digest) []string {
	out := []string{}
	if m.opt.Runs == nil {
		return out
	}
	runs, err := m.opt.Runs.ActiveRuns(ctx)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, r := range runs {
		if r.Harness != contracts.HarnessClaude && r.Harness != contracts.HarnessCodex {
			continue // shell/mock runs take no environment generation
		}
		switch sc.Kind {
		case "project":
			if r.ProjectID != sc.ProjectID {
				continue
			}
		case "session":
			if r.SessionID != sc.SessionID {
				continue
			}
		}
		if r.GenerationID != "" {
			if g, err := m.loadRecord(r.GenerationID); err == nil && g.Generation.Digest == candidate {
				continue
			}
		}
		if !seen[r.SessionID] {
			seen[r.SessionID] = true
			out = append(out, r.SessionID)
		}
	}
	sort.Strings(out)
	return out
}

// markRestart flags values whose effective value changed versus prev.
func markRestart(g, prev *contracts.EnvironmentGeneration, hot map[string]bool) {
	old := map[string]string{}
	for _, v := range prev.Values {
		old[v.Key] = v.Value
	}
	for i := range g.Values {
		v := &g.Values[i]
		if o, ok := old[v.Key]; (!ok || o != v.Value) && !hot[v.Key] {
			v.RestartRequired = true
		}
	}
}

func summarizeOverlay(rel string, b []byte) string {
	switch {
	case strings.HasSuffix(rel, "settings.json"):
		return "claude --settings overlay (" + fmt.Sprint(len(b)) + " bytes); user settings.json untouched"
	case strings.HasSuffix(rel, "mcp.json"):
		return "claude --mcp-config overlay"
	default:
		return ""
	}
}

func codexArgsDiff(prev *genRecord, now []string) string {
	var old []string
	if prev != nil {
		old = prev.Overlay.CodexArgs
	}
	return lineDiff(strings.Join(pairArgs(old), "\n"), strings.Join(pairArgs(now), "\n"))
}

func pairArgs(a []string) []string {
	var out []string
	for i := 0; i+1 < len(a); i += 2 {
		k, v, _ := strings.Cut(a[i+1], "=")
		if SensitiveKey(k) || SensitiveValue(ParseValue(v)) {
			v = Redacted
		}
		out = append(out, a[i]+" "+k+"="+v)
	}
	return out
}

// registrationChanges lists hook/MCP (un)registrations versus prev.
func registrationChanges(prev *genRecord, res *resolution) []contracts.PlanChange {
	out := append([]contracts.PlanChange{}, res.notes...)
	if prev == nil {
		return out
	}
	now := map[string]bool{}
	for _, e := range res.gen.MCPServers {
		if e.Managed {
			now[string(e.Harness)+":mcp:"+e.Name] = true
		}
	}
	for _, e := range prev.Generation.MCPServers {
		k := string(e.Harness) + ":mcp:" + e.Name
		if e.Managed && !now[k] {
			out = append(out, contracts.PlanChange{Path: k, Action: "unregister"})
		}
	}
	return out
}
