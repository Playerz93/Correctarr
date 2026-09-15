// Package db wraps the SQLite store that holds settings, arr instances,
// findings and sweep runs.
package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	s, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(1)
	d := &DB{sql: s}
	if err := d.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	_, err := d.sql.Exec(`
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS arr_instances (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	name    TEXT NOT NULL,
	type    TEXT NOT NULL DEFAULT '',
	url     TEXT NOT NULL,
	api_key TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS findings (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	key              TEXT NOT NULL UNIQUE,
	instance_id      INTEGER NOT NULL,
	instance_name    TEXT NOT NULL,
	instance_type    TEXT NOT NULL,
	media_id         INTEGER NOT NULL,
	episode_id       INTEGER NOT NULL DEFAULT 0,
	season_number    INTEGER NOT NULL DEFAULT 0,
	file_id          INTEGER NOT NULL DEFAULT 0,
	title            TEXT NOT NULL,
	path             TEXT NOT NULL,
	issue            TEXT NOT NULL,
	detail           TEXT NOT NULL DEFAULT '',
	plex_section_id  TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL,
	first_seen       TEXT NOT NULL,
	last_seen        TEXT NOT NULL,
	fix_requested_at TEXT,
	fixed_at         TEXT,
	last_action      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS findings_status ON findings(status);
CREATE TABLE IF NOT EXISTS runs (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at    TEXT NOT NULL,
	finished_at   TEXT,
	mode          TEXT NOT NULL,
	dry_run       INTEGER NOT NULL DEFAULT 0,
	depth         TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	files_checked INTEGER NOT NULL DEFAULT 0,
	broken        INTEGER NOT NULL DEFAULT 0,
	need_fix      INTEGER NOT NULL DEFAULT 0,
	fixed         INTEGER NOT NULL DEFAULT 0,
	new_broken    INTEGER NOT NULL DEFAULT 0,
	newly_fixed   INTEGER NOT NULL DEFAULT 0,
	log           TEXT NOT NULL DEFAULT ''
);
`)
	return err
}

// ---------------------------------------------------------------- settings

// Settings is the editable configuration shown in the web GUI.
type Settings struct {
	PlexURL            string `json:"plex_url"`
	PlexToken          string `json:"plex_token"`
	SweepIntervalHours int    `json:"sweep_interval_hours"`
	IntegrityDepth     string `json:"integrity_depth"` // off, quick, standard, full
	IntegrityWorkers   int    `json:"integrity_workers"`
	DryRun             bool   `json:"dry_run"`
	AutoFix            bool   `json:"auto_fix"`
	FixAllArmed        bool   `json:"fix_all_armed"`
	FixDelaySeconds    int    `json:"fix_delay_seconds"`
	FfprobeTimeoutSecs int    `json:"ffprobe_timeout_seconds"`
}

func DefaultSettings() Settings {
	return Settings{
		SweepIntervalHours: 6,
		IntegrityDepth:     "standard",
		IntegrityWorkers:   3,
		DryRun:             true,
		AutoFix:            false,
		FixAllArmed:        false,
		FixDelaySeconds:    2,
		FfprobeTimeoutSecs: 90,
	}
}

func (d *DB) GetSettings() (Settings, error) {
	s := DefaultSettings()
	rows, err := d.sql.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, err
		}
		switch k {
		case "plex_url":
			s.PlexURL = v
		case "plex_token":
			s.PlexToken = v
		case "sweep_interval_hours":
			s.SweepIntervalHours, _ = strconv.Atoi(v)
		case "integrity_depth":
			s.IntegrityDepth = v
		case "integrity_workers":
			s.IntegrityWorkers, _ = strconv.Atoi(v)
		case "dry_run":
			s.DryRun = v == "1"
		case "auto_fix":
			s.AutoFix = v == "1"
		case "fix_all_armed":
			s.FixAllArmed = v == "1"
		case "fix_delay_seconds":
			s.FixDelaySeconds, _ = strconv.Atoi(v)
		case "ffprobe_timeout_seconds":
			s.FfprobeTimeoutSecs, _ = strconv.Atoi(v)
		}
	}
	return s, rows.Err()
}

func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (d *DB) SaveSettings(s Settings) error {
	if s.SweepIntervalHours < 1 {
		s.SweepIntervalHours = 1
	}
	if s.IntegrityWorkers < 1 {
		s.IntegrityWorkers = 1
	}
	if s.FfprobeTimeoutSecs < 10 {
		s.FfprobeTimeoutSecs = 10
	}
	if s.FixDelaySeconds < 0 {
		s.FixDelaySeconds = 0
	}
	switch s.IntegrityDepth {
	case "off", "quick", "standard", "full":
	default:
		s.IntegrityDepth = "standard"
	}
	kv := map[string]string{
		"plex_url":                s.PlexURL,
		"plex_token":              s.PlexToken,
		"sweep_interval_hours":    strconv.Itoa(s.SweepIntervalHours),
		"integrity_depth":         s.IntegrityDepth,
		"integrity_workers":       strconv.Itoa(s.IntegrityWorkers),
		"dry_run":                 b2s(s.DryRun),
		"auto_fix":                b2s(s.AutoFix),
		"fix_all_armed":           b2s(s.FixAllArmed),
		"fix_delay_seconds":       strconv.Itoa(s.FixDelaySeconds),
		"ffprobe_timeout_seconds": strconv.Itoa(s.FfprobeTimeoutSecs),
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ------------------------------------------------------------ arr instances

type ArrInstance struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // sonarr, radarr or "" when not yet detected
	URL     string `json:"url"`
	APIKey  string `json:"api_key"`
	Enabled bool   `json:"enabled"`
}

func (d *DB) ListArrs() ([]ArrInstance, error) {
	rows, err := d.sql.Query(`SELECT id,name,type,url,api_key,enabled FROM arr_instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArrInstance
	for rows.Next() {
		var a ArrInstance
		var en int
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.URL, &a.APIKey, &en); err != nil {
			return nil, err
		}
		a.Enabled = en == 1
		out = append(out, a)
	}
	if out == nil {
		out = []ArrInstance{}
	}
	return out, rows.Err()
}

func (d *DB) GetArr(id int64) (ArrInstance, error) {
	var a ArrInstance
	var en int
	err := d.sql.QueryRow(`SELECT id,name,type,url,api_key,enabled FROM arr_instances WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Type, &a.URL, &a.APIKey, &en)
	a.Enabled = en == 1
	return a, err
}

func (d *DB) SaveArr(a ArrInstance) (ArrInstance, error) {
	if a.ID == 0 {
		res, err := d.sql.Exec(`INSERT INTO arr_instances(name,type,url,api_key,enabled) VALUES(?,?,?,?,?)`,
			a.Name, a.Type, a.URL, a.APIKey, b2s(a.Enabled))
		if err != nil {
			return a, err
		}
		a.ID, _ = res.LastInsertId()
		return a, nil
	}
	_, err := d.sql.Exec(`UPDATE arr_instances SET name=?,type=?,url=?,api_key=?,enabled=? WHERE id=?`,
		a.Name, a.Type, a.URL, a.APIKey, b2s(a.Enabled), a.ID)
	return a, err
}

func (d *DB) DeleteArr(id int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM findings WHERE instance_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM arr_instances WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ----------------------------------------------------------------- findings

const (
	StatusBroken = "broken" // seen broken, nothing done yet
	StatusFixing = "fixing" // a fix was triggered, waiting for the item to come back healthy
	StatusFixed  = "fixed"  // confirmed healthy after a fix (or the item left the arr)
)

const (
	IssueMissingLink     = "missing_symlink"  // arr path does not exist at all
	IssueBrokenTarget    = "broken_target"    // symlink exists, target does not
	IssueEmptyFile       = "empty_file"       // resolves but is zero bytes
	IssueIntegrityFailed = "integrity_failed" // read-through / ffprobe check failed
	IssueNotInPlex       = "not_in_plex"      // file is healthy but Plex has no item for it
)

type Finding struct {
	ID             int64      `json:"id"`
	Key            string     `json:"key"`
	InstanceID     int64      `json:"instance_id"`
	InstanceName   string     `json:"instance_name"`
	InstanceType   string     `json:"instance_type"`
	MediaID        int        `json:"media_id"`
	EpisodeID      int        `json:"episode_id"`
	SeasonNumber   int        `json:"season_number"`
	FileID         int        `json:"file_id"`
	Title          string     `json:"title"`
	Path           string     `json:"path"`
	Issue          string     `json:"issue"`
	Detail         string     `json:"detail"`
	PlexSectionID  string     `json:"plex_section_id"`
	Status         string     `json:"status"`
	FirstSeen      time.Time  `json:"first_seen"`
	LastSeen       time.Time  `json:"last_seen"`
	FixRequestedAt *time.Time `json:"fix_requested_at,omitempty"`
	FixedAt        *time.Time `json:"fixed_at,omitempty"`
	LastAction     string     `json:"last_action"`
}

const findingCols = `id,key,instance_id,instance_name,instance_type,media_id,episode_id,season_number,file_id,title,path,issue,detail,plex_section_id,status,first_seen,last_seen,fix_requested_at,fixed_at,last_action`

func scanFinding(sc interface{ Scan(...any) error }) (Finding, error) {
	var f Finding
	var first, last string
	var fixReq, fixed sql.NullString
	err := sc.Scan(&f.ID, &f.Key, &f.InstanceID, &f.InstanceName, &f.InstanceType, &f.MediaID, &f.EpisodeID,
		&f.SeasonNumber, &f.FileID, &f.Title, &f.Path, &f.Issue, &f.Detail, &f.PlexSectionID, &f.Status,
		&first, &last, &fixReq, &fixed, &f.LastAction)
	if err != nil {
		return f, err
	}
	f.FirstSeen, _ = time.Parse(time.RFC3339, first)
	f.LastSeen, _ = time.Parse(time.RFC3339, last)
	if fixReq.Valid {
		t, _ := time.Parse(time.RFC3339, fixReq.String)
		f.FixRequestedAt = &t
	}
	if fixed.Valid {
		t, _ := time.Parse(time.RFC3339, fixed.String)
		f.FixedAt = &t
	}
	return f, nil
}

func (d *DB) ListFindings(status string, instanceID int64) ([]Finding, error) {
	q := `SELECT ` + findingCols + ` FROM findings WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	if instanceID != 0 {
		q += ` AND instance_id=?`
		args = append(args, instanceID)
	}
	q += ` ORDER BY CASE status WHEN 'broken' THEN 0 WHEN 'fixing' THEN 1 ELSE 2 END, last_seen DESC, title`
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *DB) GetFinding(id int64) (Finding, error) {
	return scanFinding(d.sql.QueryRow(`SELECT `+findingCols+` FROM findings WHERE id=?`, id))
}

func (d *DB) GetFindingByKey(key string) (Finding, bool, error) {
	f, err := scanFinding(d.sql.QueryRow(`SELECT `+findingCols+` FROM findings WHERE key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	return f, err == nil, err
}

// AllFindingsByKey returns every finding indexed by key. Used by the sweep to
// reconcile the current state against what was seen before.
func (d *DB) AllFindingsByKey() (map[string]Finding, error) {
	rows, err := d.sql.Query(`SELECT ` + findingCols + ` FROM findings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Finding{}
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out[f.Key] = f
	}
	return out, rows.Err()
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func nts(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// UpsertFinding inserts or updates a finding by key. All fields are written.
func (d *DB) UpsertFinding(f Finding) (Finding, error) {
	res, err := d.sql.Exec(`
INSERT INTO findings(key,instance_id,instance_name,instance_type,media_id,episode_id,season_number,file_id,title,path,issue,detail,plex_section_id,status,first_seen,last_seen,fix_requested_at,fixed_at,last_action)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET
	instance_name=excluded.instance_name, instance_type=excluded.instance_type,
	season_number=excluded.season_number, file_id=excluded.file_id, title=excluded.title, path=excluded.path,
	issue=excluded.issue, detail=excluded.detail, plex_section_id=excluded.plex_section_id, status=excluded.status,
	first_seen=excluded.first_seen, last_seen=excluded.last_seen, fix_requested_at=excluded.fix_requested_at,
	fixed_at=excluded.fixed_at, last_action=excluded.last_action`,
		f.Key, f.InstanceID, f.InstanceName, f.InstanceType, f.MediaID, f.EpisodeID, f.SeasonNumber, f.FileID,
		f.Title, f.Path, f.Issue, f.Detail, f.PlexSectionID, f.Status, ts(f.FirstSeen), ts(f.LastSeen),
		nts(f.FixRequestedAt), nts(f.FixedAt), f.LastAction)
	if err != nil {
		return f, err
	}
	if f.ID == 0 {
		if id, err := res.LastInsertId(); err == nil && id != 0 {
			f.ID = id
		}
		if f.ID == 0 {
			got, _, err := d.GetFindingByKey(f.Key)
			if err == nil {
				f.ID = got.ID
			}
		}
	}
	return f, nil
}

func (d *DB) DeleteFinding(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM findings WHERE id=?`, id)
	return err
}

func (d *DB) ClearFixed() (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM findings WHERE status=?`, StatusFixed)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type Tally struct {
	Broken  int `json:"broken"`
	NeedFix int `json:"need_fix"`
	Fixed   int `json:"fixed"`
}

func (d *DB) Tally() (Tally, error) {
	var t Tally
	rows, err := d.sql.Query(`SELECT status, COUNT(*) FROM findings GROUP BY status`)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return t, err
		}
		switch s {
		case StatusBroken:
			t.Broken = n
		case StatusFixing:
			t.NeedFix = n
		case StatusFixed:
			t.Fixed = n
		}
	}
	return t, rows.Err()
}

// --------------------------------------------------------------------- runs

type Run struct {
	ID           int64      `json:"id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Mode         string     `json:"mode"` // scheduled, manual, dry
	DryRun       bool       `json:"dry_run"`
	Depth        string     `json:"depth"`
	Status       string     `json:"status"` // running, done, failed, cancelled
	FilesChecked int        `json:"files_checked"`
	Broken       int        `json:"broken"`
	NeedFix      int        `json:"need_fix"`
	Fixed        int        `json:"fixed"`
	NewBroken    int        `json:"new_broken"`
	NewlyFixed   int        `json:"newly_fixed"`
	Log          string     `json:"log,omitempty"`
}

func (d *DB) CreateRun(r Run) (int64, error) {
	res, err := d.sql.Exec(`INSERT INTO runs(started_at,mode,dry_run,depth,status) VALUES(?,?,?,?,?)`,
		ts(r.StartedAt), r.Mode, b2s(r.DryRun), r.Depth, r.Status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateRun(r Run) error {
	_, err := d.sql.Exec(`UPDATE runs SET finished_at=?,status=?,files_checked=?,broken=?,need_fix=?,fixed=?,new_broken=?,newly_fixed=?,log=? WHERE id=?`,
		nts(r.FinishedAt), r.Status, r.FilesChecked, r.Broken, r.NeedFix, r.Fixed, r.NewBroken, r.NewlyFixed, r.Log, r.ID)
	return err
}

const runCols = `id,started_at,finished_at,mode,dry_run,depth,status,files_checked,broken,need_fix,fixed,new_broken,newly_fixed`

func scanRun(sc interface{ Scan(...any) error }, withLog bool) (Run, error) {
	var r Run
	var started string
	var finished sql.NullString
	var dry int
	dest := []any{&r.ID, &started, &finished, &r.Mode, &dry, &r.Depth, &r.Status, &r.FilesChecked, &r.Broken, &r.NeedFix, &r.Fixed, &r.NewBroken, &r.NewlyFixed}
	if withLog {
		dest = append(dest, &r.Log)
	}
	if err := sc.Scan(dest...); err != nil {
		return r, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339, started)
	if finished.Valid {
		t, _ := time.Parse(time.RFC3339, finished.String)
		r.FinishedAt = &t
	}
	r.DryRun = dry == 1
	return r, nil
}

func (d *DB) ListRuns(limit int) ([]Run, error) {
	rows, err := d.sql.Query(`SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) GetRun(id int64) (Run, error) {
	return scanRun(d.sql.QueryRow(`SELECT `+runCols+`,log FROM runs WHERE id=?`, id), true)
}

func (d *DB) LastFinishedRun() (*Run, error) {
	r, err := scanRun(d.sql.QueryRow(`SELECT `+runCols+` FROM runs WHERE finished_at IS NOT NULL ORDER BY id DESC LIMIT 1`), false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// MarkStaleRuns flags runs left in "running" by a previous process as failed.
func (d *DB) MarkStaleRuns() error {
	now := ts(time.Now())
	_, err := d.sql.Exec(`UPDATE runs SET status='failed', finished_at=?, log=log||char(10)||'process restarted while running' WHERE status='running'`, now)
	return err
}

// Ping is used by the web layer to confirm the store is reachable.
func (d *DB) Ping(ctx context.Context) error { return d.sql.PingContext(ctx) }
