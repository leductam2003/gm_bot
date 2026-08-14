package opensea

import "testing"

// TestParseListingCurrency uses the real Robinhood collection shape: pricing_currencies
// .listing_currency = USDG (6 decimals) must be detected as the required ERC-20.
func TestParseListingCurrency(t *testing.T) {
	body := []byte(`{
		"collection":"gm-335599275",
		"pricing_currencies":{
			"listing_currency":{"symbol":"USDG","address":"0x5fc5360d0400a0fd4f2af552add042d716f1d168","chain":"robinhood","name":"Global Dollar","decimals":6},
			"offer_currency":{"symbol":"USDG","address":"0x5fc5360d0400a0fd4f2af552add042d716f1d168","decimals":6}
		}
	}`)
	cur, ok := parseListingCurrency(body)
	if !ok {
		t.Fatal("expected USDG currency to be detected")
	}
	if cur.Symbol != "USDG" || cur.Decimals != 6 || cur.Address != "0x5fc5360d0400a0fd4f2af552add042d716f1d168" {
		t.Fatalf("parsed %+v, want USDG/6/0x5fc5…", cur)
	}
}

// A collection with no pricing currency (or a native zero-address one) → list in ETH.
func TestParseListingCurrencyNative(t *testing.T) {
	if _, ok := parseListingCurrency([]byte(`{"collection":"x"}`)); ok {
		t.Fatal("no pricing_currencies must mean native (ok=false)")
	}
	zero := []byte(`{"pricing_currencies":{"listing_currency":{"address":"0x0000000000000000000000000000000000000000","decimals":18,"symbol":"ETH"}}}`)
	if _, ok := parseListingCurrency(zero); ok {
		t.Fatal("zero-address currency must mean native (ok=false)")
	}
}
