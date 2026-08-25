package opensea

import "testing"

func TestIsRateLimitBody(t *testing.T) {
	// OpenSea returns rate limits as HTTP 200 with this envelope — must be detected by body.
	rl := []string{
		`{"errors":[{"message":"Too Many Requests"}]}`,
		`{"errors":[{"message":"rate limit exceeded"}]}`,
		`{"data":{"swap":{"actions":[],"errors":[]}},"errors":[{"message":"Too Many Requests"}]}`,
	}
	for _, b := range rl {
		if !isRateLimitBody([]byte(b)) {
			t.Fatalf("should be rate-limited: %s", b)
		}
	}
	// A real ineligibility / valid response must NOT be mistaken for a rate limit.
	ok := []string{
		`{"data":{"swap":{"actions":[],"errors":[{"__typename":"InsufficientMintsRemainingError"}]}}}`,
		`{"data":{"swap":{"actions":[{"transactionSubmissionData":{"to":"0x1","data":"0x2","value":"0"}}],"errors":[]}}}`,
		`{"data":{"swap":{"actions":[],"errors":[{"__typename":"DropNotMintingError"}]}}}`,
	}
	for _, b := range ok {
		if isRateLimitBody([]byte(b)) {
			t.Fatalf("should NOT be rate-limited: %s", b)
		}
	}
}

func TestParseSwapEligibilityVsVoucher(t *testing.T) {
	// A valid voucher parses to a tx.
	v, err := parseSwap([]byte(`{"data":{"swap":{"actions":[{"transactionSubmissionData":{"to":"0x00005EA0","data":"0x4b61cd6f","value":"0"}}],"errors":[]}}}`))
	if err != nil || v.To != "0x00005EA0" || v.ValueWei != "0" {
		t.Fatalf("valid voucher: v=%+v err=%v", v, err)
	}
	// Already-minted-out is a terminal error, not retryable.
	if _, err := parseSwap([]byte(`{"data":{"swap":{"actions":[],"errors":[{"__typename":"InsufficientMintsRemainingError"}]}}}`)); err == nil {
		t.Fatal("expected error for InsufficientMintsRemainingError")
	}
}
