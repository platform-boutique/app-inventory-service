package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"inventory-service/data"
)

func TestEmbeddedSeedIsValid(t *testing.T) {
	items, err := ParseSeed(data.Stock)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 100 {
		t.Fatalf("only %d seed products", len(items))
	}
}

func TestParseSeedRejectsBadEntries(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":  `{`,
		"negative":  `[{"product_id":"a","available":-1}]`,
		"no id":     `[{"product_id":"","available":1}]`,
		"duplicate": `[{"product_id":"a","available":1},{"product_id":"a","available":2}]`,
		"overflow":  `[{"product_id":"x","available":99999999999}]`,
	} {
		if _, err := ParseSeed([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	got, err := ParseSeed([]byte(`[{"product_id":"b","available":0},{"product_id":"a","available":5}]`))
	if err != nil || got[0].ProductID != "a" || got[1].Available != 0 {
		t.Fatalf("got %v, err %v (want sorted, zero stock allowed)", got, err)
	}
}

// TestSeedMatchesCatalog fails when data/stock.json no longer matches the `stock` fields of the catalog's
// seed file. The catalog repo is a sibling checkout (apps/product-catalog-service); set CATALOG_SEED to
// the path of products.json to use another location. Without the file the test is skipped, not passed
// silently: the skip message says so. Fix a failure with `python3 tools/sync_stock.py`.
func TestSeedMatchesCatalog(t *testing.T) {
	path := os.Getenv("CATALOG_SEED")
	if path == "" {
		path = filepath.Join("..", "..", "..", "product-catalog-service", "data", "seed", "products.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("catalog seed %s not found (set CATALOG_SEED=/path/to/products.json): drift NOT checked", path)
	}
	var products []struct {
		ID    string `json:"id"`
		Stock *int   `json:"stock"`
	}
	if err := json.Unmarshal(raw, &products); err != nil {
		t.Fatalf("catalog seed: %v", err)
	}
	want := map[string]int{}
	for _, p := range products {
		if p.Stock == nil {
			t.Fatalf("catalog product %q has no stock field", p.ID)
		}
		want[p.ID] = *p.Stock
	}
	have, err := ParseSeed(data.Stock)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, s := range have {
		got[s.ProductID] = int(s.Available)
	}
	for id, w := range want {
		if g, ok := got[id]; !ok {
			t.Errorf("drift: %s is in the catalog (stock %d) but not in data/stock.json", id, w)
		} else if g != w {
			t.Errorf("drift: %s has stock %d in data/stock.json, %d in the catalog", id, g, w)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("drift: %s is in data/stock.json but no longer in the catalog", id)
		}
	}
	if t.Failed() {
		t.Log("run: python3 tools/sync_stock.py")
	}
}
