// Package arr is a small Sonarr / Radarr v3 API client covering what the
// sweep and the fix actions need.
package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	Sonarr = "sonarr"
	Radarr = "radarr"
)

type Client struct {
	Name   string
	Type   string
	URL    string
	APIKey string
	HTTP   *http.Client
}

func New(name, typ, url, apiKey string) *Client {
	return &Client{
		Name:   name,
		Type:   strings.ToLower(typ),
		URL:    strings.TrimRight(strings.TrimSpace(url), "/"),
		APIKey: strings.TrimSpace(apiKey),
		HTTP:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) (int, error) {
	if c.URL == "" {
		return 0, errors.New("url is empty")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+"/"+strings.TrimLeft(endpoint, "/"), rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, errors.New("unauthorized: check the API key")
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d %s", method, endpoint, resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s: %w", endpoint, err)
		}
	}
	return resp.StatusCode, nil
}

// Status is what /system/status returns, trimmed to the useful bits.
type Status struct {
	AppName  string `json:"appName"`
	Version  string `json:"version"`
	Instance string `json:"instanceName"`
}

// TestConnection calls /system/status and returns the detected app type.
func (c *Client) TestConnection(ctx context.Context) (Status, string, error) {
	var st Status
	if _, err := c.do(ctx, http.MethodGet, "api/v3/system/status", nil, &st); err != nil {
		return st, "", err
	}
	typ := strings.ToLower(st.AppName)
	switch typ {
	case Sonarr, Radarr:
	default:
		return st, typ, fmt.Errorf("%q is not Sonarr or Radarr", st.AppName)
	}
	return st, typ, nil
}

// File is a single media file the arr believes it owns.
type File struct {
	FileID       int
	Path         string
	Size         int64
	MediaID      int // seriesId or movieId
	Title        string
	EpisodeID    int // sonarr only
	SeasonNumber int // sonarr only
	Label        string
}

// Media is a series or movie that the arr tracks, with or without a file.
type Media struct {
	ID    int
	Title string
	Path  string // root folder of the series / movie
	Files []File
}

// ListMedia returns every series / movie with its current files.
func (c *Client) ListMedia(ctx context.Context) ([]Media, error) {
	switch c.Type {
	case Sonarr:
		return c.listSeries(ctx)
	case Radarr:
		return c.listMovies(ctx)
	}
	return nil, fmt.Errorf("unknown arr type %q, run test connection first", c.Type)
}

type movie struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Path      string `json:"path"`
	HasFile   bool   `json:"hasFile"`
	MovieFile *struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"movieFile"`
}

func (c *Client) listMovies(ctx context.Context) ([]Media, error) {
	var movies []movie
	if _, err := c.do(ctx, http.MethodGet, "api/v3/movie", nil, &movies); err != nil {
		return nil, err
	}
	out := make([]Media, 0, len(movies))
	for _, m := range movies {
		title := m.Title
		if m.Year > 0 {
			title = fmt.Sprintf("%s (%d)", m.Title, m.Year)
		}
		md := Media{ID: m.ID, Title: title, Path: m.Path}
		if m.MovieFile != nil && m.MovieFile.ID != 0 && m.MovieFile.Path != "" {
			md.Files = []File{{
				FileID:  m.MovieFile.ID,
				Path:    m.MovieFile.Path,
				Size:    m.MovieFile.Size,
				MediaID: m.ID,
				Title:   title,
				Label:   title,
			}}
		}
		out = append(out, md)
	}
	return out, nil
}

type series struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

type episodeFile struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
}

type episode struct {
	ID            int    `json:"id"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeFileID int    `json:"episodeFileId"`
	Title         string `json:"title"`
	HasFile       bool   `json:"hasFile"`
}

func (c *Client) listSeries(ctx context.Context) ([]Media, error) {
	var all []series
	if _, err := c.do(ctx, http.MethodGet, "api/v3/series", nil, &all); err != nil {
		return nil, err
	}
	out := make([]Media, 0, len(all))
	for _, s := range all {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		md := Media{ID: s.ID, Title: s.Title, Path: s.Path}
		var files []episodeFile
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("api/v3/episodefile?seriesId=%d", s.ID), nil, &files); err != nil {
			return out, fmt.Errorf("series %q: %w", s.Title, err)
		}
		if len(files) == 0 {
			out = append(out, md)
			continue
		}
		var eps []episode
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("api/v3/episode?seriesId=%d", s.ID), nil, &eps); err != nil {
			return out, fmt.Errorf("series %q episodes: %w", s.Title, err)
		}
		byFile := map[int]episode{}
		for _, e := range eps {
			if e.EpisodeFileID != 0 {
				// A multi-episode file maps to several episodes; keep the first.
				if _, ok := byFile[e.EpisodeFileID]; !ok {
					byFile[e.EpisodeFileID] = e
				}
			}
		}
		for _, f := range files {
			if f.ID == 0 || f.Path == "" {
				continue
			}
			e := byFile[f.ID]
			label := fmt.Sprintf("%s - S%02dE%02d", s.Title, f.SeasonNumber, e.EpisodeNumber)
			if e.Title != "" {
				label += " - " + e.Title
			}
			md.Files = append(md.Files, File{
				FileID:       f.ID,
				Path:         f.Path,
				Size:         f.Size,
				MediaID:      s.ID,
				Title:        s.Title,
				EpisodeID:    e.ID,
				SeasonNumber: f.SeasonNumber,
				Label:        label,
			})
		}
		out = append(out, md)
	}
	return out, nil
}

// DeleteFile removes the arr's record of a file so the item counts as missing.
// A 404 is treated as success: the record is already gone.
func (c *Client) DeleteFile(ctx context.Context, fileID int) error {
	var ep string
	switch c.Type {
	case Sonarr:
		ep = fmt.Sprintf("api/v3/episodefile/%d", fileID)
	case Radarr:
		ep = fmt.Sprintf("api/v3/moviefile/%d", fileID)
	default:
		return fmt.Errorf("unknown arr type %q", c.Type)
	}
	code, err := c.do(ctx, http.MethodDelete, ep, nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

// Search asks the arr to look for a replacement. For Sonarr an EpisodeSearch
// is used when the episode id is known, otherwise a SeasonSearch. For Radarr
// it is a MoviesSearch.
func (c *Client) Search(ctx context.Context, f File) error {
	var cmd map[string]any
	switch c.Type {
	case Sonarr:
		if f.EpisodeID != 0 {
			cmd = map[string]any{"name": "EpisodeSearch", "episodeIds": []int{f.EpisodeID}}
		} else {
			cmd = map[string]any{"name": "SeasonSearch", "seriesId": f.MediaID, "seasonNumber": f.SeasonNumber}
		}
	case Radarr:
		cmd = map[string]any{"name": "MoviesSearch", "movieIds": []int{f.MediaID}}
	default:
		return fmt.Errorf("unknown arr type %q", c.Type)
	}
	_, err := c.do(ctx, http.MethodPost, "api/v3/command", cmd, nil)
	return err
}

// GetFile returns the arr's current file for one movie or episode. ok is
// false when the item exists but has no file (for example while a search is
// still running). An error is returned when the item itself is gone.
func (c *Client) GetFile(ctx context.Context, mediaID, episodeID int) (File, bool, error) {
	switch c.Type {
	case Radarr:
		var m movie
		code, err := c.do(ctx, http.MethodGet, fmt.Sprintf("api/v3/movie/%d", mediaID), nil, &m)
		if code == http.StatusNotFound {
			return File{}, false, fmt.Errorf("movie %d no longer exists in %s", mediaID, c.Name)
		}
		if err != nil {
			return File{}, false, err
		}
		title := m.Title
		if m.Year > 0 {
			title = fmt.Sprintf("%s (%d)", m.Title, m.Year)
		}
		if m.MovieFile == nil || m.MovieFile.ID == 0 || m.MovieFile.Path == "" {
			return File{MediaID: m.ID, Title: title, Label: title}, false, nil
		}
		return File{FileID: m.MovieFile.ID, Path: m.MovieFile.Path, Size: m.MovieFile.Size, MediaID: m.ID, Title: title, Label: title}, true, nil
	case Sonarr:
		var s series
		code, err := c.do(ctx, http.MethodGet, fmt.Sprintf("api/v3/series/%d", mediaID), nil, &s)
		if code == http.StatusNotFound {
			return File{}, false, fmt.Errorf("series %d no longer exists in %s", mediaID, c.Name)
		}
		if err != nil {
			return File{}, false, err
		}
		var eps []episode
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("api/v3/episode?seriesId=%d", mediaID), nil, &eps); err != nil {
			return File{}, false, err
		}
		var ep *episode
		for i := range eps {
			if eps[i].ID == episodeID {
				ep = &eps[i]
				break
			}
		}
		if ep == nil {
			return File{}, false, fmt.Errorf("episode %d no longer exists in %s", episodeID, c.Name)
		}
		label := fmt.Sprintf("%s - S%02dE%02d", s.Title, ep.SeasonNumber, ep.EpisodeNumber)
		if ep.Title != "" {
			label += " - " + ep.Title
		}
		f := File{MediaID: s.ID, Title: s.Title, EpisodeID: ep.ID, SeasonNumber: ep.SeasonNumber, Label: label}
		if ep.EpisodeFileID == 0 {
			return f, false, nil
		}
		var ef episodeFile
		code, err = c.do(ctx, http.MethodGet, fmt.Sprintf("api/v3/episodefile/%d", ep.EpisodeFileID), nil, &ef)
		if code == http.StatusNotFound || ef.Path == "" {
			return f, false, nil
		}
		if err != nil {
			return File{}, false, err
		}
		f.FileID, f.Path, f.Size = ef.ID, ef.Path, ef.Size
		return f, true, nil
	}
	return File{}, false, fmt.Errorf("unknown arr type %q", c.Type)
}
