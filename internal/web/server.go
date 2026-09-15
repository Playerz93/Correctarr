// Package web serves the JSON API and the embedded single-page GUI.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/playerz93/correctarr/internal/arr"
	"github.com/playerz93/correctarr/internal/db"
	"github.com/playerz93/correctarr/internal/plex"
	"github.com/playerz93/correctarr/internal/sweep"
)

//go:embed static/*
var static embed.FS

type Server struct {
	db      *db.DB
	sweep   *sweep.Service
	log     *log.Logger
	version string
}

func New(store *db.DB, svc *sweep.Service, logger *log.Logger, version string) http.Handler {
	s := &Server{db: store, sweep: svc, log: logger, version: version}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("GET /api/arrs", s.listArrs)
	mux.HandleFunc("POST /api/arrs", s.saveArr)
	mux.HandleFunc("DELETE /api/arrs/{id}", s.deleteArr)
	mux.HandleFunc("POST /api/arrs/test", s.testArr)
	mux.HandleFunc("POST /api/plex/test", s.testPlex)
	mux.HandleFunc("GET /api/findings", s.listFindings)
	mux.HandleFunc("POST /api/findings/{id}/fix", s.fixFinding)
	mux.HandleFunc("DELETE /api/findings/{id}", s.deleteFinding)
	mux.HandleFunc("POST /api/findings/fix-all", s.fixAll)
	mux.HandleFunc("POST /api/findings/clear-fixed", s.clearFixed)
	mux.HandleFunc("POST /api/sweep", s.startSweep)
	mux.HandleFunc("POST /api/sweep/cancel", s.cancelSweep)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)

	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if r.URL.Path == "/" {
			// FileServer would redirect /index.html back to /, so serve it directly.
			http.ServeFileFS(w, r, sub, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	}))
	return logRequests(mux, logger)
}

func logRequests(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
			logger.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// ------------------------------------------------------------------- status

type statusResp struct {
	Version       string       `json:"version"`
	Tally         db.Tally     `json:"tally"`
	Sweep         sweep.Status `json:"sweep"`
	LastRun       *db.Run      `json:"last_run,omitempty"`
	DryRun        bool         `json:"dry_run"`
	AutoFix       bool         `json:"auto_fix"`
	FixAllArmed   bool         `json:"fix_all_armed"`
	PlexSet       bool         `json:"plex_configured"`
	ArrCount      int          `json:"arr_count"`
	IntegrityMode string       `json:"integrity_depth"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.Tally()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	settings, _ := s.db.GetSettings()
	arrs, _ := s.db.ListArrs()
	last, _ := s.db.LastFinishedRun()
	n := 0
	for _, a := range arrs {
		if a.Enabled {
			n++
		}
	}
	writeJSON(w, 200, statusResp{
		Version: s.version, Tally: t, Sweep: s.sweep.Status(), LastRun: last,
		DryRun: settings.DryRun, AutoFix: settings.AutoFix, FixAllArmed: settings.FixAllArmed,
		PlexSet: settings.PlexURL != "" && settings.PlexToken != "", ArrCount: n, IntegrityMode: settings.IntegrityDepth,
	})
}

// ----------------------------------------------------------------- settings

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.db.GetSettings()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var st db.Settings
	if err := readJSON(r, &st); err != nil {
		writeErr(w, 400, err)
		return
	}
	st.PlexURL = strings.TrimRight(strings.TrimSpace(st.PlexURL), "/")
	st.PlexToken = strings.TrimSpace(st.PlexToken)
	if err := s.db.SaveSettings(st); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.sweep.Reschedule()
	saved, _ := s.db.GetSettings()
	writeJSON(w, 200, saved)
}

// --------------------------------------------------------------------- arrs

func (s *Server) listArrs(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.ListArrs()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) saveArr(w http.ResponseWriter, r *http.Request) {
	var a db.ArrInstance
	if err := readJSON(r, &a); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.Name = strings.TrimSpace(a.Name)
	a.URL = strings.TrimRight(strings.TrimSpace(a.URL), "/")
	a.APIKey = strings.TrimSpace(a.APIKey)
	a.Type = strings.ToLower(strings.TrimSpace(a.Type))
	if a.Name == "" || a.URL == "" || a.APIKey == "" {
		writeErr(w, 400, errors.New("name, url and api key are required"))
		return
	}
	if a.Type != "" && a.Type != arr.Sonarr && a.Type != arr.Radarr {
		writeErr(w, 400, errors.New("type must be sonarr, radarr or empty"))
		return
	}
	saved, err := s.db.SaveArr(a)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.sweep.Reschedule()
	writeJSON(w, 200, saved)
}

func (s *Server) deleteArr(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.db.DeleteArr(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) testArr(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	c := arr.New("test", "", in.URL, in.APIKey)
	st, typ, err := c.TestConnection(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "type": typ})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "type": typ, "app": st.AppName, "version": st.Version, "instance": st.Instance})
}

func (s *Server) testPlex(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	c := plex.New(in.URL, in.Token)
	id, secs, err := c.TestConnection(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if secs == nil {
		secs = []plex.Section{}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": id.Name, "version": id.Version, "platform": id.Platform, "sections": secs})
}

// ----------------------------------------------------------------- findings

func (s *Server) listFindings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var inst int64
	if v := q.Get("instance"); v != "" {
		inst, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := s.db.ListFindings(q.Get("status"), inst)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) fixFinding(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	_ = readJSON(r, &in)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	msg, err := s.sweep.Fix(ctx, id, in.Action)
	if err != nil {
		code := 500
		if errors.Is(err, sweep.ErrRunning) {
			code = 409
		}
		writeErr(w, code, err)
		return
	}
	f, _ := s.db.GetFinding(id)
	writeJSON(w, 200, map[string]any{"ok": true, "message": msg, "finding": f})
}

func (s *Server) deleteFinding(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.db.DeleteFinding(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) fixAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := s.sweep.FixAll(ctx)
	if err != nil {
		code := 400
		if errors.Is(err, sweep.ErrRunning) {
			code = 409
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) clearFixed(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.ClearFixed()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "cleared": n})
}

// -------------------------------------------------------------------- sweep

func (s *Server) startSweep(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode  string `json:"mode"`
		Depth string `json:"depth"`
	}
	_ = readJSON(r, &in)
	mode := sweep.ModeManual
	if in.Mode == sweep.ModeDry {
		mode = sweep.ModeDry
	}
	switch in.Depth {
	case "", "off", "quick", "standard", "full":
	default:
		writeErr(w, 400, errors.New("bad depth"))
		return
	}
	id, err := s.sweep.Start(sweep.Request{Mode: mode, Depth: in.Depth})
	if err != nil {
		code := 500
		if errors.Is(err, sweep.ErrRunning) {
			code = 409
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "run_id": id})
}

func (s *Server) cancelSweep(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"cancelled": s.sweep.Cancel()})
}

// --------------------------------------------------------------------- runs

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.ListRuns(50)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	run, err := s.db.GetRun(id)
	if err != nil {
		writeErr(w, 404, errors.New("run not found"))
		return
	}
	writeJSON(w, 200, run)
}
