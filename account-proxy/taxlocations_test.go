package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNinjaTaxLocationsCA(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ninja/ws/CA/tax_locations?state=&lang=en&shop_id=1&_type=json", nil)
	handleNinjaShop(w, r)
	var out struct {
		TaxLocations struct {
			TaxLocation []struct {
				ID         int    `json:"id"`
				City       string `json:"city"`
				County     string `json:"county"`
				State      string `json:"state"`
				PostalCode string `json:"postal_code"`
			} `json:"tax_location"`
		} `json:"tax_locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
	entries := out.TaxLocations.TaxLocation
	if len(entries) != len(caProvinces) {
		t.Fatalf("got %d provinces, want %d", len(entries), len(caProvinces))
	}
	for i, e := range entries {
		if e.State != caProvinces[i] {
			t.Errorf("entry %d state=%q want %q", i, e.State, caProvinces[i])
		}
		if len(e.State) > 3 || len(e.City) > 26 || len(e.County) > 16 || len(e.PostalCode) > 6 {
			t.Errorf("entry %d exceeds mint's field length limits: %+v", i, e)
		}
	}
}

func TestNinjaTaxLocationsUS(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ninja/ws/US/tax_locations?postal_code=90210&lang=en&shop_id=1&_type=json", nil)
	handleNinjaShop(w, r)
	var out struct {
		TaxLocations struct {
			TaxLocation []struct {
				State      string `json:"state"`
				PostalCode string `json:"postal_code"`
			} `json:"tax_location"`
		} `json:"tax_locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
	entries := out.TaxLocations.TaxLocation
	if len(entries) != 1 {
		t.Fatalf("got %d entries for a real ZIP, want exactly 1", len(entries))
	}
	if entries[0].PostalCode != "90210" {
		t.Errorf("postal_code echoed as %q, want 90210", entries[0].PostalCode)
	}
	if entries[0].State != "CA" {
		t.Errorf("state=%q for ZIP 90210, want CA (state name collides with country code CA - just a coincidence, this is the US state)", entries[0].State)
	}
}

func TestNinjaTaxLocationsUSNoZip(t *testing.T) {
	// mint never sends this in practice, but the handler must not crash or return a
	// malformed array if it ever does.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ninja/ws/US/tax_locations?lang=en&shop_id=1&_type=json", nil)
	handleNinjaShop(w, r)
	var out struct {
		TaxLocations struct {
			TaxLocation []interface{} `json:"tax_location"`
		} `json:"tax_locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
	if len(out.TaxLocations.TaxLocation) != 0 {
		t.Errorf("expected an empty (but valid) array with no postal_code, got %d entries", len(out.TaxLocations.TaxLocation))
	}
}

func TestNinjaTaxLocationPut(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/ninja/ws/my/tax_location/!put?_type=json", strings.NewReader("tax_location_id=0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handleNinjaShop(w, r)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
}
