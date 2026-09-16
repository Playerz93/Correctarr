// Package sweep compares what the arrs own with what is on disk and in Plex,
// records findings, and carries out fix actions.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/playerz93/correctarr/internal/arr"
	"github.com/playerz93/correctarr/internal/db"
	"github.com/playerz93/correctarr/internal/integrity"
	"github.com/playerz93/correctarr/internal/plex"
)

const (
	ModeScheduled = "scheduled"
	ModeManual    = "manual"
	ModeDry       = "dry"

	ActionAuto     = "auto"
	ActionResearch = "research"
	ActionPlexScan = "plexscan"
)

var ErrRunning = errors.New("a sweep is already running")

// Progress is the live state of the current run.
type Progress struct {
	RunID   int64  `json:"run_id"`
	Mode    string `json:"mode"`
	DryRun  bool   `json:"dry_run"`
	Phase   string `json:"phase"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current"`
	Started time.Time
}

type Status struct {
	Running   bool       `json:"running"`
	Progress  *Progress  `json:"progress,omitempty"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// Request describes a sweep to start.
type Request struct {
	Mode   string
	DryRun *bool  // nil = use settings
	Depth  string // "" = use settings
}

type Service struct {
	db  *db.DB
	log *log.Logger

	mu       sync.Mutex
	progress *Progress
	cancel   context.CancelFunc
	nextRun  *time.Time
	wake     chan struct{}
	fixMu    sync.Mutex
}

func New(store *db.DB, logger *log.Logger) *Service {
	return &Service{db: store, log: logger, wake: make(chan struct{}, 1)}
}

// Status returns the current run progress and the next scheduled time.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{NextRunAt: s.nextRun}
	if s.progress != nil {
		p := *s.progress
		st.Running = true
		st.Progress = &p
	}
	return st
}

// Reschedule tells the scheduler that settings changed.
func (s *Service) Reschedule() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// RunScheduler blocks and starts scheduled sweeps until ctx is done.
func (s *Service) RunScheduler(ctx context.Context) {
	for {
		next := s.computeNext()
		s.mu.Lock()
		s.nextRun = &next
		s.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		arrs, _ := s.db.ListArrs()
		enabled := 0
		for _, a := range arrs {
			if a.Enabled {
				enabled++
			}
		}
		if enabled == 0 {
			s.log.Printf("scheduler: no enabled arr instances, skipping sweep")
			continue
		}
		if _, err := s.Start(Request{Mode: ModeScheduled}); err != nil && !errors.Is(err, ErrRunning) {
			s.log.Printf("scheduler: %v", err)
		}
		// Wait for the run to finish before computing the next slot.
		for {
			time.Sleep(2 * time.Second)
			if !s.Status().Running || ctx.Err() != nil {
				break
			}
		}
	}
}

func (s *Service) computeNext() time.Time {
	settings, _ := s.db.GetSettings()
	interval := time.Duration(settings.SweepIntervalHours) * time.Hour
	if interval < time.Hour {
		interval = time.Hour
	}
	now := time.Now()
	last, _ := s.db.LastFinishedRun()
	if last == nil {
		return now.Add(2 * time.Minute)
	}
	next := last.StartedAt.Add(interval)
	if next.Before(now.Add(time.Minute)) {
		next = now.Add(time.Minute)
	}
	return next
}

// Start begins a sweep in the background.
func (s *Service) Start(req Request) (int64, error) {
	s.mu.Lock()
	if s.progress != nil {
		s.mu.Unlock()
		return 0, ErrRunning
	}
	settings, err := s.db.GetSettings()
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	// Manual sweeps never fix anything, so dry-run only matters for the
	// scheduled auto-fix and for an explicit dry run.
	dry := false
	if req.Mode == ModeScheduled {
		dry = settings.AutoFixDryRun
	}
	if req.DryRun != nil {
		dry = *req.DryRun
	}
	if req.Mode == ModeDry {
		dry = true
	}
	depth := settings.IntegrityDepth
	if req.Depth != "" {
		depth = req.Depth
	}
	run := db.Run{StartedAt: time.Now(), Mode: req.Mode, DryRun: dry, Depth: depth, Status: "running"}
	id, err := s.db.CreateRun(run)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	run.ID = id
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.progress = &Progress{RunID: id, Mode: req.Mode, DryRun: dry, Phase: "starting", Started: run.StartedAt}
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.progress = nil
			s.cancel = nil
			s.mu.Unlock()
			cancel()
		}()
		s.execute(ctx, run, settings, depth)
	}()
	return id, nil
}

// Cancel stops the running sweep, if any.
func (s *Service) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		return false
	}
	s.cancel()
	return true
}

func (s *Service) setPhase(phase string, done, total int, current string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progress == nil {
		return
	}
	s.progress.Phase = phase
	s.progress.Done = done
	s.progress.Total = total
	s.progress.Current = current
}

// runLog collects the per-run log shown in the GUI.
type runLog struct {
	mu  sync.Mutex
	b   strings.Builder
	svc *Service
}

func (l *runLog) Printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.svc.log.Printf("sweep: %s", line)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() < 400<<10 {
		l.b.WriteString(time.Now().Format("15:04:05 "))
		l.b.WriteString(line)
		l.b.WriteByte('\n')
	}
}

func (l *runLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fileItem is one arr file plus what we learned about it.
type fileItem struct {
	inst   db.ArrInstance
	file   arr.File
	issue  string
	detail string
}

func keyFor(instID int64, f arr.File) string {
	return fmt.Sprintf("%d:%d:%d", instID, f.MediaID, f.EpisodeID)
}

func (s *Service) execute(ctx context.Context, run db.Run, settings db.Settings, depth string) {
	lg := &runLog{svc: s}
	finish := func(status string) {
		now := time.Now()
		run.FinishedAt = &now
		run.Status = status
		run.Log = lg.String()
		if t, err := s.db.Tally(); err == nil {
			run.Broken, run.NeedFix, run.Fixed = t.Broken, t.NeedFix, t.Fixed
		}
		if err := s.db.UpdateRun(run); err != nil {
			s.log.Printf("sweep: save run: %v", err)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			lg.Printf("PANIC: %v", r)
			finish("failed")
		}
	}()

	lg.Printf("run %d started (mode=%s, dry_run=%v, integrity=%s)", run.ID, run.Mode, run.DryRun, depth)
	if (depth == integrity.DepthStandard || depth == integrity.DepthFull) && !integrity.HaveFfprobe() {
		lg.Printf("WARNING: ffprobe not found in PATH, falling back to quick integrity checks")
		depth = integrity.DepthQuick
		run.Depth = depth
	}

	// 1. Plex index.
	var ix *plex.Index
	var plexClient *plex.Client
	if settings.PlexURL != "" && settings.PlexToken != "" {
		s.setPhase("indexing plex", 0, 0, settings.PlexURL)
		plexClient = plex.New(settings.PlexURL, settings.PlexToken)
		var err error
		ix, err = plexClient.BuildIndex(ctx)
		if err != nil {
			if ctx.Err() != nil {
				lg.Printf("cancelled")
				finish("cancelled")
				return
			}
			lg.Printf("plex: %v (Plex checks skipped this run)", err)
			ix = nil
		} else {
			lg.Printf("plex: indexed %d files across %d sections", len(ix.Paths), len(ix.Sections))
		}
	} else {
		lg.Printf("plex: not configured, Plex checks skipped")
	}

	// 2. Arr listings.
	arrs, err := s.db.ListArrs()
	if err != nil {
		lg.Printf("db: %v", err)
		finish("failed")
		return
	}
	var items []fileItem
	listed := map[int64]bool{}
	for _, a := range arrs {
		if !a.Enabled {
			continue
		}
		s.setPhase("listing "+a.Name, 0, 0, a.URL)
		c := arr.New(a.Name, a.Type, a.URL, a.APIKey)
		if c.Type == "" {
			_, typ, err := c.TestConnection(ctx)
			if err != nil {
				lg.Printf("%s: %v (skipped)", a.Name, err)
				continue
			}
			c.Type = typ
			a.Type = typ
			_, _ = s.db.SaveArr(a)
		}
		media, err := c.ListMedia(ctx)
		if err != nil {
			if ctx.Err() != nil {
				lg.Printf("cancelled")
				finish("cancelled")
				return
			}
			lg.Printf("%s: %v (skipped)", a.Name, err)
			continue
		}
		n := 0
		for _, m := range media {
			for _, f := range m.Files {
				items = append(items, fileItem{inst: a, file: f})
				n++
			}
		}
		listed[a.ID] = true
		lg.Printf("%s (%s): %d items, %d files", a.Name, c.Type, len(media), n)
	}
	if len(listed) == 0 {
		lg.Printf("no arr instance could be listed")
		finish("failed")
		return
	}

	// 3. Disk + integrity + Plex checks.
	total := len(items)
	workers := settings.IntegrityWorkers
	if workers < 1 {
		workers = 1
	}
	var (
		wg   sync.WaitGroup
		done int
		dmu  sync.Mutex
		jobs = make(chan int)
	)
	probeOpts := integrity.Options{Depth: depth, FfprobeTimeout: time.Duration(settings.FfprobeTimeoutSecs) * time.Second}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				it := &items[i]
				checkItem(ctx, it, ix, probeOpts)
				dmu.Lock()
				done++
				d := done
				dmu.Unlock()
				s.setPhase("checking files", d, total, it.file.Path)
			}
		}()
	}
	s.setPhase("checking files", 0, total, "")
loop:
	for i := range items {
		select {
		case <-ctx.Done():
			break loop
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		lg.Printf("cancelled after %d of %d files", done, total)
		finish("cancelled")
		return
	}
	run.FilesChecked = total

	// 4. Reconcile with stored findings.
	s.setPhase("reconciling", 0, 0, "")
	existing, err := s.db.AllFindingsByKey()
	if err != nil {
		lg.Printf("db: %v", err)
		finish("failed")
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	byIssue := map[string]int{}
	var newBroken, newlyFixed int
	var toFix []db.Finding
	for _, it := range items {
		k := keyFor(it.inst.ID, it.file)
		seen[k] = true
		old, had := existing[k]
		if it.issue == "" {
			if had && old.Status != db.StatusFixed {
				old.Status = db.StatusFixed
				old.FixedAt = &now
				old.LastSeen = now
				old.Path = it.file.Path
				old.FileID = it.file.FileID
				if old.FixRequestedAt == nil {
					old.LastAction = "recovered without action"
				} else {
					old.LastAction = "confirmed healthy after fix"
				}
				old.Detail = "healthy"
				if _, err := s.db.UpsertFinding(old); err != nil {
					lg.Printf("db: %v", err)
				}
				newlyFixed++
				lg.Printf("FIXED  %s: %s", it.inst.Name, it.file.Label)
			}
			continue
		}
		byIssue[it.issue]++
		f := db.Finding{
			Key: k, InstanceID: it.inst.ID, InstanceName: it.inst.Name, InstanceType: it.inst.Type,
			MediaID: it.file.MediaID, EpisodeID: it.file.EpisodeID, SeasonNumber: it.file.SeasonNumber,
			FileID: it.file.FileID, Title: it.file.Label, Path: it.file.Path, Issue: it.issue, Detail: it.detail,
			Status: db.StatusBroken, FirstSeen: now, LastSeen: now,
		}
		if ix != nil {
			want := "movie"
			if it.inst.Type == arr.Sonarr {
				want = "show"
			}
			f.PlexSectionID = ix.SectionFor(it.file.Path, want)
		}
		switch {
		case !had:
			newBroken++
			lg.Printf("BROKEN %s: %s [%s] %s", it.inst.Name, it.file.Label, it.issue, it.detail)
		case old.Status == db.StatusFixed:
			newBroken++
			f.LastAction = "reopened"
			lg.Printf("REOPEN %s: %s [%s] %s", it.inst.Name, it.file.Label, it.issue, it.detail)
		default:
			f.ID = old.ID
			f.Status = old.Status
			f.FirstSeen = old.FirstSeen
			f.FixRequestedAt = old.FixRequestedAt
			f.LastAction = old.LastAction
			if old.Status == db.StatusFixing && old.FileID != it.file.FileID && it.file.FileID != 0 {
				f.LastAction = old.LastAction + " (new file grabbed, still broken)"
			}
		}
		saved, err := s.db.UpsertFinding(f)
		if err != nil {
			lg.Printf("db: %v", err)
			continue
		}
		if saved.Status == db.StatusBroken {
			toFix = append(toFix, saved)
		}
	}
	// Findings whose media was listed but not seen: the file record is gone.
	for k, old := range existing {
		if seen[k] || !listed[old.InstanceID] {
			continue
		}
		switch old.Status {
		case db.StatusBroken:
			old.Status = db.StatusFixed
			old.FixedAt = &now
			old.LastSeen = now
			old.LastAction = "file record removed from arr"
			if _, err := s.db.UpsertFinding(old); err != nil {
				lg.Printf("db: %v", err)
			}
			newlyFixed++
			lg.Printf("GONE   %s: %s (no longer has a file in the arr)", old.InstanceName, old.Title)
		case db.StatusFixing:
			// Waiting for the arr to grab a replacement; leave as is.
		}
	}
	run.NewBroken = newBroken
	run.NewlyFixed = newlyFixed
	var parts []string
	for _, k := range sortedKeys(byIssue) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, byIssue[k]))
	}
	lg.Printf("checked %d files: %d unhealthy (%s), %d new, %d newly fixed", total, len(toFix)+countFixing(existing, seen), strings.Join(parts, ", "), newBroken, newlyFixed)

	// 5. Fixes.
	switch {
	case run.Mode == ModeDry, run.Mode == ModeScheduled && settings.AutoFix:
		if len(toFix) > 0 {
			s.setPhase("fixing", 0, len(toFix), "")
			label := "auto-fix"
			if run.DryRun {
				label = "dry run"
			}
			lg.Printf("%s: %d findings to act on", label, len(toFix))
			for i, f := range toFix {
				if ctx.Err() != nil {
					break
				}
				msg, err := s.applyFix(ctx, f, ActionAuto, run.DryRun, settings, plexClient)
				if err != nil {
					lg.Printf("FIX    %s: %s -> error: %v", f.InstanceName, f.Title, err)
				} else {
					lg.Printf("FIX    %s: %s -> %s", f.InstanceName, f.Title, msg)
				}
				s.setPhase("fixing", i+1, len(toFix), f.Title)
				if !run.DryRun && settings.FixDelaySeconds > 0 && i < len(toFix)-1 {
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(settings.FixDelaySeconds) * time.Second):
					}
				}
			}
		} else {
			lg.Printf("nothing to fix")
		}
	case run.Mode == ModeManual && len(toFix) > 0:
		lg.Printf("%d broken findings waiting: use Fix selected, Fix all, or the per-row buttons", len(toFix))
	}
	if ctx.Err() != nil {
		finish("cancelled")
		return
	}
	lg.Printf("run %d finished in %s", run.ID, time.Since(run.StartedAt).Round(time.Second))
	finish("done")
}

func countFixing(existing map[string]db.Finding, seen map[string]bool) int {
	n := 0
	for k, f := range existing {
		if seen[k] && f.Status == db.StatusFixing {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkItem fills in issue/detail for one file.
func checkItem(ctx context.Context, it *fileItem, ix *plex.Index, opt integrity.Options) {
	d := integrity.CheckDisk(it.file.Path)
	switch {
	case !d.Exists:
		it.issue = db.IssueMissingLink
		it.detail = d.StatError
		return
	case !d.Resolves:
		it.issue = db.IssueBrokenTarget
		if d.Target != "" {
			it.detail = "symlink target " + d.Target + ": " + d.StatError
		} else {
			it.detail = d.StatError
		}
		return
	case d.Size == 0:
		it.issue = db.IssueEmptyFile
		it.detail = "zero bytes"
		return
	}
	opt.ExpectedSize = it.file.Size
	if r := integrity.Check(ctx, it.file.Path, opt); !r.OK {
		if ctx.Err() != nil {
			return
		}
		it.issue = db.IssueIntegrityFailed
		it.detail = r.Depth + ": " + r.Detail
		return
	}
	if ix != nil && !ix.Has(it.file.Path) {
		it.issue = db.IssueNotInPlex
		it.detail = "file is healthy but Plex has no item with this path"
	}
}

// Fix carries out one fix action for a finding. With dry set the action is
// only described.
func (s *Service) Fix(ctx context.Context, findingID int64, action string, dry bool) (string, error) {
	if s.Status().Running {
		return "", ErrRunning
	}
	s.fixMu.Lock()
	defer s.fixMu.Unlock()
	f, err := s.db.GetFinding(findingID)
	if err != nil {
		return "", fmt.Errorf("finding not found")
	}
	settings, err := s.db.GetSettings()
	if err != nil {
		return "", err
	}
	var pc *plex.Client
	if settings.PlexURL != "" && settings.PlexToken != "" {
		pc = plex.New(settings.PlexURL, settings.PlexToken)
	}
	return s.applyFix(ctx, f, action, dry, settings, pc)
}

// FixAllResult summarises a fix-all pass.
type FixAllResult struct {
	Attempted int      `json:"attempted"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	DryRun    bool     `json:"dry_run"`
	Lines     []string `json:"lines"`
}

// FixAll applies the automatic fix to every finding in the broken state, or,
// when ids are given, to exactly those findings (any state except fixed).
// The fix-all arm switch is only required for the "everything" form.
func (s *Service) FixAll(ctx context.Context, ids []int64, dry bool) (FixAllResult, error) {
	res := FixAllResult{DryRun: dry}
	if s.Status().Running {
		return res, ErrRunning
	}
	s.fixMu.Lock()
	defer s.fixMu.Unlock()
	settings, err := s.db.GetSettings()
	if err != nil {
		return res, err
	}
	var list []db.Finding
	if len(ids) == 0 {
		if !settings.FixAllArmed && !dry {
			return res, errors.New("fix all is not armed: enable it in settings first")
		}
		list, err = s.db.ListFindings(db.StatusBroken, 0)
		if err != nil {
			return res, err
		}
	} else {
		for _, id := range ids {
			f, err := s.db.GetFinding(id)
			if err != nil {
				res.Lines = append(res.Lines, fmt.Sprintf("finding %d: not found", id))
				continue
			}
			if f.Status == db.StatusFixed {
				res.Lines = append(res.Lines, fmt.Sprintf("%s: %s -> already fixed, skipped", f.InstanceName, f.Title))
				continue
			}
			list = append(list, f)
		}
	}
	var pc *plex.Client
	if settings.PlexURL != "" && settings.PlexToken != "" {
		pc = plex.New(settings.PlexURL, settings.PlexToken)
	}
	for i, f := range list {
		if ctx.Err() != nil {
			break
		}
		res.Attempted++
		msg, err := s.applyFix(ctx, f, ActionAuto, dry, settings, pc)
		if err != nil {
			res.Failed++
			res.Lines = append(res.Lines, fmt.Sprintf("%s: %s -> error: %v", f.InstanceName, f.Title, err))
		} else {
			res.Succeeded++
			res.Lines = append(res.Lines, fmt.Sprintf("%s: %s -> %s", f.InstanceName, f.Title, msg))
		}
		if !dry && settings.FixDelaySeconds > 0 && i < len(list)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(settings.FixDelaySeconds) * time.Second):
			}
		}
	}
	return res, nil
}

// Recheck re-verifies one finding right now: asks the arr for the item's
// current file, checks disk, integrity and Plex, and updates the finding.
func (s *Service) Recheck(ctx context.Context, findingID int64) (db.Finding, string, error) {
	if s.Status().Running {
		return db.Finding{}, "", ErrRunning
	}
	s.fixMu.Lock()
	defer s.fixMu.Unlock()
	f, err := s.db.GetFinding(findingID)
	if err != nil {
		return f, "", fmt.Errorf("finding not found")
	}
	settings, err := s.db.GetSettings()
	if err != nil {
		return f, "", err
	}
	inst, err := s.db.GetArr(f.InstanceID)
	if err != nil {
		return f, "", fmt.Errorf("arr instance %d not found", f.InstanceID)
	}
	now := time.Now()
	c := arr.New(inst.Name, inst.Type, inst.URL, inst.APIKey)
	file, hasFile, err := c.GetFile(ctx, f.MediaID, f.EpisodeID)
	if err != nil {
		// The item itself is gone from the arr: nothing left to fix.
		f.Status = db.StatusFixed
		f.FixedAt = &now
		f.LastSeen = now
		f.LastAction = "item removed from arr: " + err.Error()
		f.Detail = err.Error()
		f, _ = s.db.UpsertFinding(f)
		return f, "item no longer exists in the arr, marked fixed", nil
	}
	if !hasFile {
		f.LastSeen = now
		f.Detail = "arr has no file for this item yet"
		if f.Status == db.StatusBroken {
			// The file record went away without our doing; treat as resolved.
			f.Status = db.StatusFixed
			f.FixedAt = &now
			f.LastAction = "file record removed from arr"
		}
		f, err = s.db.UpsertFinding(f)
		return f, "arr has no file for this item yet (still searching?)", err
	}

	// Plex index for just the section this path belongs to.
	var ix *plex.Index
	if settings.PlexURL != "" && settings.PlexToken != "" {
		pc := plex.New(settings.PlexURL, settings.PlexToken)
		if secs, err := pc.Sections(ctx); err == nil {
			all := &plex.Index{Sections: secs}
			want := "movie"
			if inst.Type == arr.Sonarr {
				want = "show"
			}
			if key := all.SectionFor(file.Path, want); key != "" {
				for _, sec := range secs {
					if sec.Key == key {
						ix, _ = pc.IndexSection(ctx, sec)
					}
				}
				f.PlexSectionID = key
			}
		}
	}
	depth := settings.IntegrityDepth
	if (depth == integrity.DepthStandard || depth == integrity.DepthFull) && !integrity.HaveFfprobe() {
		depth = integrity.DepthQuick
	}
	it := fileItem{inst: inst, file: file}
	checkItem(ctx, &it, ix, integrity.Options{Depth: depth, FfprobeTimeout: time.Duration(settings.FfprobeTimeoutSecs) * time.Second})

	f.Path = file.Path
	f.FileID = file.FileID
	f.Title = file.Label
	f.LastSeen = now
	if it.issue == "" {
		if f.Status != db.StatusFixed {
			f.Status = db.StatusFixed
			f.FixedAt = &now
			if f.FixRequestedAt == nil {
				f.LastAction = "healthy on re-check"
			} else {
				f.LastAction = "confirmed healthy after fix"
			}
		}
		f.Detail = "healthy"
		f, err = s.db.UpsertFinding(f)
		return f, "healthy", err
	}
	f.Issue = it.issue
	f.Detail = it.detail
	if f.Status == db.StatusFixed {
		f.Status = db.StatusBroken
		f.FirstSeen = now
		f.FixRequestedAt = nil
		f.FixedAt = nil
		f.LastAction = "reopened on re-check"
	}
	f, err = s.db.UpsertFinding(f)
	return f, "still " + it.issue + ": " + it.detail, err
}

func chooseAction(f db.Finding) string {
	if f.Issue == db.IssueNotInPlex {
		return ActionPlexScan
	}
	return ActionResearch
}

// applyFix performs (or, in dry run, describes) the action and updates the finding.
func (s *Service) applyFix(ctx context.Context, f db.Finding, action string, dryRun bool, settings db.Settings, pc *plex.Client) (string, error) {
	if action == "" || action == ActionAuto {
		action = chooseAction(f)
	}
	now := time.Now()
	var describe string
	var do func() error

	switch action {
	case ActionResearch:
		inst, err := s.db.GetArr(f.InstanceID)
		if err != nil {
			return "", fmt.Errorf("arr instance %d not found", f.InstanceID)
		}
		c := arr.New(inst.Name, inst.Type, inst.URL, inst.APIKey)
		file := arr.File{FileID: f.FileID, MediaID: f.MediaID, EpisodeID: f.EpisodeID, SeasonNumber: f.SeasonNumber}
		if f.FileID != 0 {
			describe = fmt.Sprintf("delete file record %d in %s and search for a replacement", f.FileID, inst.Name)
		} else {
			describe = fmt.Sprintf("search for a replacement in %s", inst.Name)
		}
		do = func() error {
			if f.FileID != 0 {
				if err := c.DeleteFile(ctx, f.FileID); err != nil {
					return fmt.Errorf("delete file: %w", err)
				}
			}
			if err := c.Search(ctx, file); err != nil {
				return fmt.Errorf("search: %w", err)
			}
			return nil
		}
	case ActionPlexScan:
		if pc == nil {
			return "", errors.New("plex is not configured")
		}
		section := f.PlexSectionID
		dir := filepath.Dir(f.Path)
		if section == "" {
			// Try to resolve now; the finding may predate Plex being configured.
			secs, err := pc.Sections(ctx)
			if err != nil {
				return "", err
			}
			ix := &plex.Index{Sections: secs}
			want := "movie"
			if f.InstanceType == arr.Sonarr {
				want = "show"
			}
			section = ix.SectionFor(f.Path, want)
			f.PlexSectionID = section
		}
		describe = fmt.Sprintf("ask Plex to scan section %s at %s", section, dir)
		do = func() error { return pc.ScanPath(ctx, section, dir) }
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}

	if dryRun {
		f.LastAction = "dry run: would " + describe
		f.LastSeen = now
		if _, err := s.db.UpsertFinding(f); err != nil {
			return "", err
		}
		return "would " + describe, nil
	}
	if err := do(); err != nil {
		f.LastAction = fmt.Sprintf("%s failed: %v", action, err)
		f.LastSeen = now
		_, _ = s.db.UpsertFinding(f)
		return "", err
	}
	f.Status = db.StatusFixing
	f.FixRequestedAt = &now
	f.LastSeen = now
	f.LastAction = describe
	if _, err := s.db.UpsertFinding(f); err != nil {
		return "", err
	}
	return describe, nil
}
