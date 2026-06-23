package evm

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestGweiToWei(t *testing.T) {
	cases := map[float64]string{
		0.1:   "100000000",
		2.0:   "2000000000",
		0.001: "1000000",
		1.5:   "1500000000",
		30:    "30000000000",
	}
	for g, want := range cases {
		if got := gweiToWei(g).String(); got != want {
			t.Errorf("gweiToWei(%v) = %s, want %s", g, got, want)
		}
	}
}

func TestMulRat(t *testing.T) {
	// baseFee 100 * 1.5 = 150 (integer rational, round(1.5*1000)=1500; 100*1500/1000).
	if got := mulRat(big.NewInt(100), 1.5).String(); got != "150" {
		t.Errorf("mulRat(100,1.5)=%s want 150", got)
	}
	// 1 gwei * 2.0 = 2 gwei.
	if got := mulRat(big.NewInt(1_000_000_000), 2.0).String(); got != "2000000000" {
		t.Errorf("mulRat(1e9,2.0)=%s want 2e9", got)
	}
}

func TestBuildCalldataMintUint(t *testing.T) {
	w := common.HexToAddress("0x0000000000000000000000000000000000000001")
	data, err := BuildCalldata(false, "", "mint(uint256)", []string{"3"}, w)
	if err != nil {
		t.Fatal(err)
	}
	got := "0x" + hex.EncodeToString(data)
	// selector mint(uint256)=0xa0712d68, arg 3 padded to 32 bytes.
	want := "0xa0712d680000000000000000000000000000000000000000000000000000000000000003"
	if got != want {
		t.Fatalf("calldata=%s\nwant     =%s", got, want)
	}
}

func TestBuildCalldataAddressSubstitution(t *testing.T) {
	w := common.HexToAddress("0x52908400098527886E0F7030069857D2E4169EE7")
	data, err := BuildCalldata(false, "", "transfer(address,uint256)", []string{"{address}", "100"}, w)
	if err != nil {
		t.Fatal(err)
	}
	got := "0x" + hex.EncodeToString(data)
	// transfer(address,uint256)=0xa9059cbb; arg0 = wallet left-padded; arg1 = 100 (0x64).
	want := "0xa9059cbb" +
		"00000000000000000000000052908400098527886e0f7030069857d2e4169ee7" +
		"0000000000000000000000000000000000000000000000000000000000000064"
	if got != want {
		t.Fatalf("calldata=%s\nwant     =%s", got, want)
	}
}

func TestBuildCalldataHexMode(t *testing.T) {
	w := common.Address{}
	data, err := BuildCalldata(true, "0xdeadbeef", "", nil, w)
	if err != nil {
		t.Fatal(err)
	}
	if "0x"+hex.EncodeToString(data) != "0xdeadbeef" {
		t.Fatalf("hex mode passthrough failed: %x", data)
	}
}

func TestBuildCalldataArityMismatch(t *testing.T) {
	w := common.Address{}
	if _, err := BuildCalldata(false, "", "mint(uint256)", []string{"1", "2"}, w); err == nil {
		t.Fatal("expected arity mismatch error")
	}
}

func TestBuildCalldataUint8Overflow(t *testing.T) {
	w := common.Address{}
	// 300 does not fit uint8 — must error, not silently truncate to 44.
	if _, err := BuildCalldata(false, "", "f(uint8)", []string{"300"}, w); err == nil {
		t.Fatal("expected uint8 overflow error for value 300")
	}
	// 200 fits uint8.
	if _, err := BuildCalldata(false, "", "f(uint8)", []string{"200"}, w); err != nil {
		t.Fatalf("200 should fit uint8: %v", err)
	}
}

func TestBuildCalldataNegativeUint(t *testing.T) {
	w := common.Address{}
	if _, err := BuildCalldata(false, "", "f(uint256)", []string{"-1"}, w); err == nil {
		t.Fatal("expected error for negative uint")
	}
}

func TestDecodeRevertString(t *testing.T) {
	// Error("nope") encoded.
	data, _ := hexDecode("08c379a0" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000004" +
		"6e6f706500000000000000000000000000000000000000000000000000000000")
	if got := DecodeRevert(data); got != "nope" {
		t.Fatalf("DecodeRevert=%q want nope", got)
	}
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

func TestBuildAndSignListing(t *testing.T) {
	key, _ := gethcrypto.GenerateKey()
	offerer := gethcrypto.PubkeyToAddress(key.PublicKey)
	nft := common.HexToAddress("0x9c890d7e4d9becb20f7b612d5df3c4157a0837dc")
	price := big.NewInt(1_000_000_000_000_000_000) // 1 ETH
	fees := []Fee{{Recipient: "0x0000a26b00c1F0DF003000390027140000fAa719", Bps: 250}} // 2.5%
	salt := big.NewInt(12345)

	lst, digest, err := buildAndSignListing(key, 1, big.NewInt(0), nft, big.NewInt(2102), price, fees, 30*86400, 1782000000, salt, OrderZone{})
	if err != nil {
		t.Fatal(err)
	}
	// signature recovers to the offerer → the EIP-712 digest is internally correct.
	sig, err := hexDecode(lst.Signature[2:])
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		t.Fatalf("bad signature shape len=%d v=%d", len(sig), sig[64])
	}
	rec := make([]byte, 65)
	copy(rec, sig)
	rec[64] -= 27
	pub, err := gethcrypto.SigToPub(digest, rec)
	if err != nil {
		t.Fatal(err)
	}
	if gethcrypto.PubkeyToAddress(*pub) != offerer {
		t.Fatalf("signature does not recover to offerer")
	}
	// fee math: seller gets 0.975 ETH, OS fee 0.025 ETH.
	cons := lst.Parameters["consideration"].([]interface{})
	if len(cons) != 2 {
		t.Fatalf("expected 2 consideration items, got %d", len(cons))
	}
	seller := cons[0].(map[string]interface{})
	if seller["recipient"].(string) != offerer.Hex() {
		t.Fatalf("first consideration must be the seller")
	}
	if seller["startAmount"].(string) != "975000000000000000" {
		t.Fatalf("seller amount = %v, want 0.975 ETH", seller["startAmount"])
	}
	osfee := cons[1].(map[string]interface{})
	if osfee["startAmount"].(string) != "25000000000000000" {
		t.Fatalf("OS fee = %v, want 0.025 ETH", osfee["startAmount"])
	}
	if lst.Protocol != Seaport16 {
		t.Fatalf("wrong protocol address")
	}
}

func TestSeaportIncrementCounterSelector(t *testing.T) {
	data := BuildIncrementCounter()
	sel := "0x" + hex.EncodeToString(data[:4])
	if sel != "0x5b34b966" { // incrementCounter()
		t.Fatalf("incrementCounter selector = %s, want 0x5b34b966", sel)
	}
}

func TestSeaDropMintPublicSelector(t *testing.T) {
	// mintPublic(address,address,address,uint256) selector must be 0x161ac21f.
	data, err := seaDropABI.Pack("mintPublic",
		common.HexToAddress("0x1111111111111111111111111111111111111111"),
		common.HexToAddress("0x0000a26b00c1F0DF003000390027140000fAa719"),
		common.HexToAddress(zeroAddr),
		big.NewInt(2))
	if err != nil {
		t.Fatal(err)
	}
	sel := "0x" + hex.EncodeToString(data[:4])
	if sel != "0x161ac21f" {
		t.Fatalf("mintPublic selector = %s, want 0x161ac21f", sel)
	}
}
