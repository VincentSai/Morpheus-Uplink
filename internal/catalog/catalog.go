// Package catalog maps human model names to blockchain model IDs using the
// active.mor.org gateway catalog JSON (gateway_models.json by default).
// List/open pricing uses each model's bidDetail from that file (primary).
// GATEWAY_BIDS_URL (gateway_bids.json) is the companion bid feed for
// mirrors/ops; it is not merged into LowestBid (different schema).
package catalog

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type BidDetail struct {
	BidID           string  `json:"bidId"`
	ProviderID      string  `json:"providerId"`
	PricePerSecond  string  `json:"pricePerSecond"`
	PriceMorPerHour float64 `json:"priceMorPerHour"`
	Status          string  `json:"status"`
}

type Model struct {
	Name      string      `json:"Name"`
	ID        string      `json:"Id"`
	Tags      []string    `json:"Tags"`
	ModelType string      `json:"ModelType"`
	BidDetail []BidDetail `json:"bidDetail"`
}

// LowestBid returns the cheapest healthy bid (by pricePerSecond), or any
// positive bid if none are marked healthy. ok=false when no usable bid.
func (m Model) LowestBid() (BidDetail, bool) {
	var best BidDetail
	var bestPPS *big.Int
	pick := func(onlyHealthy bool) {
		for _, b := range m.BidDetail {
			if onlyHealthy {
				if b.Status != "" && !strings.EqualFold(b.Status, "healthy") {
					continue
				}
			}
			pps, ok := new(big.Int).SetString(strings.TrimSpace(b.PricePerSecond), 10)
			if !ok || pps.Sign() <= 0 {
				continue
			}
			if bestPPS == nil || pps.Cmp(bestPPS) < 0 {
				bestPPS = pps
				best = b
			}
		}
	}
	pick(true)
	if bestPPS == nil {
		pick(false)
	}
	return best, bestPPS != nil
}

// LowestMorPerHour is the cheapest bid's catalog MOR/hour (display only).
func (m Model) LowestMorPerHour() float64 {
	b, ok := m.LowestBid()
	if !ok {
		return 0
	}
	return b.PriceMorPerHour
}

// LowestPricePerSecondWei is the cheapest bid's price/second in wei.
func (m Model) LowestPricePerSecondWei() *big.Int {
	b, ok := m.LowestBid()
	if !ok {
		return nil
	}
	n, ok := new(big.Int).SetString(strings.TrimSpace(b.PricePerSecond), 10)
	if !ok {
		return nil
	}
	return n
}

type activeModelsFile struct {
	Models []Model `json:"models"`
}

type Catalog struct {
	url     string // ACTIVE_MODELS_URL / gateway_models (bidDetail primary for list/open)
	bidsURL string // GATEWAY_BIDS_URL companion feed; not merged into LowestBid
	client  *http.Client

	mu          sync.Mutex
	models      []Model
	byName      map[string]string   // lowercase name -> id
	byID        map[string]string   // lowercase id -> display name
	twinsByName map[string][]string // lowercase name -> all candidate model ids
	fetchedAt   time.Time
	ttl         time.Duration
}

func New(url string) *Catalog {
	return &Catalog{
		url:    url,
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    5 * time.Minute,
	}
}

// WithBidsURL records the companion gateway_bids feed URL (GATEWAY_BIDS_URL).
// Catalog refresh still uses model bidDetail only; see package comment.
func (c *Catalog) WithBidsURL(bidsURL string) *Catalog {
	c.bidsURL = bidsURL
	return c
}

// GatewayBidsURL returns the companion bids feed URL, if set.
func (c *Catalog) GatewayBidsURL() string { return c.bidsURL }

func (c *Catalog) refresh() error {
	resp, err := c.client.Get(c.url)
	if err != nil {
		return fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch model catalog: status %d", resp.StatusCode)
	}
	var file activeModelsFile
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return fmt.Errorf("parse model catalog: %w", err)
	}
	byName := make(map[string]string, len(file.Models))
	byID := make(map[string]string, len(file.Models))
	twinsByName := make(map[string][]string, len(file.Models))
	bestModelByName := make(map[string]Model, len(file.Models))

	for _, m := range file.Models {
		nameKey := strings.ToLower(m.Name)
		idKey := strings.ToLower(m.ID)
		byID[idKey] = m.Name
		twinsByName[nameKey] = append(twinsByName[nameKey], m.ID)

		if existing, exists := bestModelByName[nameKey]; !exists {
			bestModelByName[nameKey] = m
			byName[nameKey] = m.ID
		} else if isBetterModel(m, existing) {
			bestModelByName[nameKey] = m
			byName[nameKey] = m.ID
		}
	}
	c.models = file.Models
	c.byName = byName
	c.byID = byID
	c.twinsByName = twinsByName
	c.fetchedAt = time.Now()
	return nil
}

// isBetterModel returns true if candidate should be chosen over current for resolving
// duplicate model names. It prioritizes healthy bids, lower price per second, and
// falls back to deterministic ID comparison to prevent feed-order randomness.
func isBetterModel(candidate, current Model) bool {
	candBid, candOk := candidate.LowestBid()
	currBid, currOk := current.LowestBid()

	if candOk && !currOk {
		return true
	}
	if !candOk && currOk {
		return false
	}
	if !candOk && !currOk {
		return strings.ToLower(candidate.ID) < strings.ToLower(current.ID)
	}

	candHealthy := candBid.Status == "" || strings.EqualFold(candBid.Status, "healthy")
	currHealthy := currBid.Status == "" || strings.EqualFold(currBid.Status, "healthy")

	if candHealthy && !currHealthy {
		return true
	}
	if !candHealthy && currHealthy {
		return false
	}

	candPPS := candidate.LowestPricePerSecondWei()
	currPPS := current.LowestPricePerSecondWei()

	if candPPS != nil && currPPS != nil {
		cmp := candPPS.Cmp(currPPS)
		if cmp < 0 {
			return true
		}
		if cmp > 0 {
			return false
		}
	} else if candPPS != nil && currPPS == nil {
		return true
	} else if candPPS == nil && currPPS != nil {
		return false
	}

	// Tie-break deterministically by ID
	return strings.ToLower(candidate.ID) < strings.ToLower(current.ID)
}

func (c *Catalog) ensureFresh() error {
	if time.Since(c.fetchedAt) < c.ttl && c.models != nil {
		return nil
	}
	err := c.refresh()
	// Serve stale data over failing hard if we have any.
	if err != nil && c.models != nil {
		return nil
	}
	return err
}

// Resolve maps a model name (or an already-resolved 0x id) to a blockchain
// model ID.
func (c *Catalog) Resolve(nameOrID string) (string, error) {
	if strings.HasPrefix(nameOrID, "0x") {
		return nameOrID, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return "", err
	}
	if id, ok := c.byName[strings.ToLower(nameOrID)]; ok {
		return id, nil
	}
	return "", fmt.Errorf("model %q not found in catalog", nameOrID)
}

func (c *Catalog) List() ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return nil, err
	}
	out := make([]Model, len(c.models))
	copy(out, c.models)
	return out, nil
}

// NameByID returns the catalog display name for a blockchain model id.
// Empty string if unknown.
func (c *Catalog) NameByID(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return ""
	}
	return c.byID[strings.ToLower(id)]
}

// TwinsByName returns all candidate blockchain model IDs registered under the given display name.
func (c *Catalog) TwinsByName(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureFresh(); err != nil {
		return nil
	}
	ids := c.twinsByName[strings.ToLower(name)]
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

