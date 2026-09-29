package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCatalog_ResolveBasic(t *testing.T) {
	mockFile := activeModelsFile{
		Models: []Model{
			{
				Name: "llama-3-8b",
				ID:   "0x1111111111111111111111111111111111111111",
				BidDetail: []BidDetail{
					{
						BidID:          "bid-1",
						PricePerSecond: "1000",
						Status:         "healthy",
					},
				},
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mockFile)
	}))
	defer ts.Close()

	cat := New(ts.URL)

	// 1. Resolve by exact name
	id, err := cat.Resolve("llama-3-8b")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if id != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected id: %s", id)
	}

	// 2. Resolve by case-insensitive name
	id, err = cat.Resolve("LLAMA-3-8B")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if id != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected id: %s", id)
	}

	// 3. Resolve by 0x address passthrough
	id, err = cat.Resolve("0xabc")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if id != "0xabc" {
		t.Fatalf("expected 0xabc passthrough, got %s", id)
	}

	// 4. Model not found
	_, err = cat.Resolve("nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent model")
	}
}

// TestCatalog_DuplicateModelNames_PicksCheapestHealthy verifies Issue #41 fix:
// when two models share a name, Resolve() must pick the cheaper healthy twin,
// rather than blindly picking whichever appeared last in the feed.
func TestCatalog_DuplicateModelNames_PicksCheapestHealthy(t *testing.T) {
	cheapID := "0xc2c4b037ff12e0aa81178deac52aeed902b36189b9e6feae22b72324c9221130"
	expensiveID := "0xe79d9b0e5022c58535cf4373bd4b74ed4ca04dea76772b845bbe87a18c529e9c"

	// Scenario 1: Cheap model is first, expensive model is last in feed
	feed1 := activeModelsFile{
		Models: []Model{
			{
				Name: "deepseek-v4-flash",
				ID:   cheapID,
				BidDetail: []BidDetail{
					{
						BidID:          "bid-cheap",
						PricePerSecond: "10400000000000000", // ~0.0104 MOR/h
						Status:         "healthy",
					},
				},
			},
			{
				Name: "deepseek-v4-flash",
				ID:   expensiveID, // Pricier twin (appears last)
				BidDetail: []BidDetail{
					{
						BidID:          "bid-expensive",
						PricePerSecond: "95000000000000000", // ~0.095 MOR/h (9x higher!)
						Status:         "healthy",
					},
				},
			},
		},
	}

	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(feed1)
	}))
	defer ts1.Close()

	cat1 := New(ts1.URL)
	resolvedID1, err := cat1.Resolve("deepseek-v4-flash")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resolvedID1 != cheapID {
		t.Fatalf("Issue #41 regression! Expected cheaper model %s, but got expensive %s", cheapID, resolvedID1)
	}

	// Scenario 2: Expensive model is first, cheap model is last in feed
	feed2 := activeModelsFile{
		Models: []Model{
			{
				Name: "deepseek-v4-flash",
				ID:   expensiveID,
				BidDetail: []BidDetail{
					{
						BidID:          "bid-expensive",
						PricePerSecond: "95000000000000000",
						Status:         "healthy",
					},
				},
			},
			{
				Name: "deepseek-v4-flash",
				ID:   cheapID,
				BidDetail: []BidDetail{
					{
						BidID:          "bid-cheap",
						PricePerSecond: "10400000000000000",
						Status:         "healthy",
					},
				},
			},
		},
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(feed2)
	}))
	defer ts2.Close()

	cat2 := New(ts2.URL)
	resolvedID2, err := cat2.Resolve("deepseek-v4-flash")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resolvedID2 != cheapID {
		t.Fatalf("Expected cheaper model %s regardless of feed order, got %s", cheapID, resolvedID2)
	}

	// Verify TwinsByName returns both candidate IDs
	twins := cat2.TwinsByName("deepseek-v4-flash")
	if len(twins) != 2 {
		t.Fatalf("expected 2 twins, got %d", len(twins))
	}
}

func TestCatalog_DuplicateModelNames_HealthyPriority(t *testing.T) {
	healthyID := "0xhealthy000000000000000000000000000000000"
	unhealthyID := "0xunhealthy0000000000000000000000000000000"

	// Unhealthy has cheaper price (e.g. 500) but status is failed/unhealthy
	// Healthy has higher price (1000) but status is healthy.
	feed := activeModelsFile{
		Models: []Model{
			{
				Name: "glm-5.2",
				ID:   healthyID,
				BidDetail: []BidDetail{
					{
						BidID:          "bid-healthy",
						PricePerSecond: "1000",
						Status:         "healthy",
					},
				},
			},
			{
				Name: "glm-5.2",
				ID:   unhealthyID,
				BidDetail: []BidDetail{
					{
						BidID:          "bid-broken",
						PricePerSecond: "500",
						Status:         "failed",
					},
				},
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(feed)
	}))
	defer ts.Close()

	cat := New(ts.URL)
	resolvedID, err := cat.Resolve("glm-5.2")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resolvedID != healthyID {
		t.Fatalf("expected healthy model %s, got %s", healthyID, resolvedID)
	}
}
