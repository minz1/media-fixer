package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Delete-and-re-add is the second owner-approved escalation, alongside
// arr_remove_and_search. It targets the case that one does not: the content
// exists in decypharr but its debrid links are broken in a way a repair sweep
// won't fix, so the torrent has to be removed from the debrid provider and
// added back to be re-fetched.
//
// Same plan/execute split as arr_replace.go, for the same reason — the owner
// approves a concrete plan, and the execute step is irreversible.

// ReaddPlan is what a delete-and-re-add would do, resolved against live
// decypharr state and rendered for owner approval before anything is removed.
// Tagged explicitly for the same reason ReplacePlan is: a plan is persisted
// between the owner's preview and their approval.
type ReaddPlan struct {
	Name     string `json:"name"`
	InfoHash string `json:"info_hash"`
	Category string `json:"category"`
	// Magnet is what the torrent will be re-added from. PlanTorrentReadd
	// refuses to produce a plan without it — see its doc comment.
	Magnet string `json:"magnet"`
	State  string `json:"state"`
	Debrid string `json:"debrid"`
}

// ReaddResult reports what ExecuteTorrentReadd actually did. On a partial
// failure it reflects everything completed before the error.
type ReaddResult struct {
	Deleted bool   `json:"deleted"`
	Readded bool   `json:"readded"`
	Name    string `json:"name"`
	Magnet  string `json:"magnet"`
}

// ErrNoMagnet is returned when a torrent has no magnet stored, so deleting it
// would be unrecoverable.
var ErrNoMagnet = errors.New("torrent has no stored magnet link")

// PlanTorrentReadd resolves name to exactly one torrent and checks it can
// actually be put back.
//
// The magnet check is the point of the whole plan step. decypharr stores the
// magnet per entry (storage.Entry.Magnet, `json:"magnet,omitempty"`), but it
// is omitted for entries that arrived some other way — and a delete with no
// magnet to re-add from is a one-way operation with no undo. Refusing here is
// the difference between "nothing happened" and "the content is gone".
//
// Requires an exact, unambiguous name match for the same reason
// findFuzzyTitle refuses short or ambiguous titles: this resolves to something
// that gets deleted.
func (c *DecypharrClient) PlanTorrentReadd(ctx context.Context, name string) (*ReaddPlan, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, errors.New("decypharr readd: name is required")
	}

	torrents, err := c.ListTorrents(ctx, trimmed, "")
	if err != nil {
		return nil, err
	}

	var match *TorrentEntry
	for _, t := range torrents {
		if !strings.EqualFold(strings.TrimSpace(t.Name), trimmed) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("decypharr readd: %q matches more than one torrent", trimmed)
		}
		match = t
	}
	if match == nil {
		return nil, fmt.Errorf("decypharr readd: no torrent named %q: %w", trimmed, ErrNotFound)
	}
	if strings.TrimSpace(match.Magnet) == "" {
		return nil, fmt.Errorf("decypharr readd: %q cannot be re-added: %w", match.Name, ErrNoMagnet)
	}

	return &ReaddPlan{
		Name:     match.Name,
		InfoHash: match.InfoHash,
		Category: match.Category,
		Magnet:   match.Magnet,
		State:    match.State,
		Debrid:   match.Debrid,
	}, nil
}

// ExecuteTorrentReadd deletes the torrent (including from the debrid provider)
// and adds it straight back from the magnet the plan captured.
//
// Ordering is deliberate: the delete must land before the add, or the provider
// returns the same cached entry and nothing is actually re-fetched. That makes
// the window between them the dangerous part, which is why the plan refuses
// without a magnet rather than discovering the problem here.
func (c *DecypharrClient) ExecuteTorrentReadd(ctx context.Context, plan *ReaddPlan) (*ReaddResult, error) {
	result := &ReaddResult{Name: plan.Name, Magnet: plan.Magnet}
	if strings.TrimSpace(plan.Magnet) == "" {
		return result, ErrNoMagnet
	}

	if err := c.DeleteTorrent(ctx, plan.Category, plan.InfoHash, true); err != nil {
		return result, fmt.Errorf("decypharr readd: delete %s: %w", plan.InfoHash, err)
	}
	result.Deleted = true

	if err := c.AddTorrent(ctx, plan.Magnet, plan.Category); err != nil {
		// The content is gone at this point and the caller must be told
		// precisely that, with the magnet, so it can be re-added by hand.
		return result, fmt.Errorf(
			"decypharr readd: deleted %q but re-adding it failed (%w); re-add manually from: %s",
			plan.Name, err, plan.Magnet)
	}
	result.Readded = true
	return result, nil
}

// AddTorrent submits a magnet to decypharr's qBittorrent-compatible API,
// which is where its add endpoint lives (`/api/v2` + `/torrents/add`, mounted
// in decypharr's own server.go) rather than under its native `/api`. The
// handler parses a form, taking the magnet as `urls` and the arr category as
// `category`.
func (c *DecypharrClient) AddTorrent(ctx context.Context, magnet, category string) error {
	form := url.Values{}
	form.Set("urls", magnet)
	if category != "" {
		form.Set("category", category)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v2/torrents/add", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("decypharr add torrent: status %d", resp.StatusCode)
	}
	return nil
}
