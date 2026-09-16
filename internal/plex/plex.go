// Package plex is a minimal Plex Media Server client: identity, library
// sections, the file paths of every item, and a folder-scoped scan.
package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		URL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token: strings.TrimSpace(token),
		HTTP:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if c.URL == "" {
		return errors.New("plex url is empty")
	}
	if q == nil {
		q = url.Values{}
	}
	u := c.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	req.Header.Set("X-Plex-Client-Identifier", "correctarr")
	req.Header.Set("X-Plex-Product", "Correctarr")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("unauthorized: check the Plex token")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

type Identity struct {
	Name     string `json:"friendlyName"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

type Section struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Type      string   `json:"type"` // movie, show, artist, photo
	Locations []string `json:"locations"`
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var resp struct {
		MediaContainer struct {
			FriendlyName string `json:"friendlyName"`
			Version      string `json:"version"`
			Platform     string `json:"platform"`
		} `json:"MediaContainer"`
	}
	if err := c.get(ctx, "/", nil, &resp); err != nil {
		return Identity{}, err
	}
	return Identity{Name: resp.MediaContainer.FriendlyName, Version: resp.MediaContainer.Version, Platform: resp.MediaContainer.Platform}, nil
}

func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var resp struct {
		MediaContainer struct {
			Directory []struct {
				Key      string `json:"key"`
				Title    string `json:"title"`
				Type     string `json:"type"`
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.get(ctx, "/library/sections", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Section, 0, len(resp.MediaContainer.Directory))
	for _, d := range resp.MediaContainer.Directory {
		s := Section{Key: d.Key, Title: d.Title, Type: d.Type}
		for _, l := range d.Location {
			s.Locations = append(s.Locations, l.Path)
		}
		out = append(out, s)
	}
	return out, nil
}

// TestConnection returns the server identity and the video sections.
func (c *Client) TestConnection(ctx context.Context) (Identity, []Section, error) {
	id, err := c.Identity(ctx)
	if err != nil {
		return id, nil, err
	}
	secs, err := c.Sections(ctx)
	if err != nil {
		return id, nil, err
	}
	var video []Section
	for _, s := range secs {
		if s.Type == "movie" || s.Type == "show" {
			video = append(video, s)
		}
	}
	return id, video, nil
}

// Index maps every file path Plex knows to the section that owns it.
type Index struct {
	Sections []Section
	Paths    map[string]string // file path -> section key
}

// Has reports whether Plex has an item for the given path.
func (ix *Index) Has(path string) bool {
	_, ok := ix.Paths[path]
	return ok
}

// SectionFor picks the section whose location contains the path; falls back
// to the only section of the wanted type, else "".
func (ix *Index) SectionFor(path, wantType string) string {
	best := ""
	bestLen := 0
	for _, s := range ix.Sections {
		for _, loc := range s.Locations {
			l := strings.TrimRight(loc, "/") + "/"
			if strings.HasPrefix(path, l) && len(l) > bestLen {
				best, bestLen = s.Key, len(l)
			}
		}
	}
	if best != "" {
		return best
	}
	var only string
	n := 0
	for _, s := range ix.Sections {
		if s.Type == wantType {
			only = s.Key
			n++
		}
	}
	if n == 1 {
		return only
	}
	return ""
}

const pageSize = 500

// BuildIndex lists every movie and episode in every video section and
// records their file paths.
func (c *Client) BuildIndex(ctx context.Context) (*Index, error) {
	secs, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}
	ix := &Index{Paths: map[string]string{}}
	for _, s := range secs {
		var typ string
		switch s.Type {
		case "movie":
			typ = "1"
		case "show":
			typ = "4" // episodes
		default:
			continue
		}
		ix.Sections = append(ix.Sections, s)
		if err := c.indexSection(ctx, s.Key, typ, ix); err != nil {
			return nil, fmt.Errorf("section %q: %w", s.Title, err)
		}
	}
	return ix, nil
}

// IndexSection lists the file paths of one section only.
func (c *Client) IndexSection(ctx context.Context, sec Section) (*Index, error) {
	ix := &Index{Paths: map[string]string{}, Sections: []Section{sec}}
	typ := "1"
	if sec.Type == "show" {
		typ = "4"
	}
	if err := c.indexSection(ctx, sec.Key, typ, ix); err != nil {
		return nil, err
	}
	return ix, nil
}

func (c *Client) indexSection(ctx context.Context, key, typ string, ix *Index) error {
	start := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var resp struct {
			MediaContainer struct {
				Size      int `json:"size"`
				TotalSize int `json:"totalSize"`
				Metadata  []struct {
					Media []struct {
						Part []struct {
							File string `json:"file"`
						} `json:"Part"`
					} `json:"Media"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		q := url.Values{}
		q.Set("type", typ)
		q.Set("X-Plex-Container-Start", strconv.Itoa(start))
		q.Set("X-Plex-Container-Size", strconv.Itoa(pageSize))
		if err := c.get(ctx, "/library/sections/"+key+"/all", q, &resp); err != nil {
			return err
		}
		mc := resp.MediaContainer
		for _, m := range mc.Metadata {
			for _, md := range m.Media {
				for _, p := range md.Part {
					if p.File != "" {
						ix.Paths[p.File] = key
					}
				}
			}
		}
		start += len(mc.Metadata)
		if len(mc.Metadata) == 0 || start >= mc.TotalSize {
			return nil
		}
	}
}

// ScanPath asks Plex to refresh one section limited to a folder.
func (c *Client) ScanPath(ctx context.Context, sectionKey, dir string) error {
	if sectionKey == "" {
		return errors.New("no plex section matched this path")
	}
	q := url.Values{}
	q.Set("path", dir)
	return c.get(ctx, "/library/sections/"+sectionKey+"/refresh", q, nil)
}
