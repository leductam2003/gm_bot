package evm

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Encodes Seaport matchAdvancedOrders calldata to ACCEPT an OpenSea offer. OpenSea's
// /offers/fulfillment_data returns the decoded args (input_data); we re-encode them with
// the Seaport ABI so the seller can broadcast the fulfillment on-chain.

var matchAdvancedOrdersABI = mustABI(`[{"type":"function","name":"matchAdvancedOrders","stateMutability":"payable","inputs":[
 {"name":"orders","type":"tuple[]","components":[
   {"name":"parameters","type":"tuple","components":[
     {"name":"offerer","type":"address"},
     {"name":"zone","type":"address"},
     {"name":"offer","type":"tuple[]","components":[
       {"name":"itemType","type":"uint8"},{"name":"token","type":"address"},
       {"name":"identifierOrCriteria","type":"uint256"},{"name":"startAmount","type":"uint256"},{"name":"endAmount","type":"uint256"}]},
     {"name":"consideration","type":"tuple[]","components":[
       {"name":"itemType","type":"uint8"},{"name":"token","type":"address"},
       {"name":"identifierOrCriteria","type":"uint256"},{"name":"startAmount","type":"uint256"},{"name":"endAmount","type":"uint256"},{"name":"recipient","type":"address"}]},
     {"name":"orderType","type":"uint8"},
     {"name":"startTime","type":"uint256"},
     {"name":"endTime","type":"uint256"},
     {"name":"zoneHash","type":"bytes32"},
     {"name":"salt","type":"uint256"},
     {"name":"conduitKey","type":"bytes32"},
     {"name":"totalOriginalConsiderationItems","type":"uint256"}]},
   {"name":"numerator","type":"uint120"},
   {"name":"denominator","type":"uint120"},
   {"name":"signature","type":"bytes"},
   {"name":"extraData","type":"bytes"}]},
 {"name":"criteriaResolvers","type":"tuple[]","components":[
   {"name":"orderIndex","type":"uint256"},{"name":"side","type":"uint8"},
   {"name":"index","type":"uint256"},{"name":"identifier","type":"uint256"},{"name":"criteriaProof","type":"bytes32[]"}]},
 {"name":"fulfillments","type":"tuple[]","components":[
   {"name":"offerComponents","type":"tuple[]","components":[{"name":"orderIndex","type":"uint256"},{"name":"itemIndex","type":"uint256"}]},
   {"name":"considerationComponents","type":"tuple[]","components":[{"name":"orderIndex","type":"uint256"},{"name":"itemIndex","type":"uint256"}]}]},
 {"name":"recipient","type":"address"}],"outputs":[]}]`)

// --- Go structs matching the ABI tuple components (field names = ToCamelCase of component) ---

type spOfferItem struct {
	ItemType             uint8
	Token                common.Address
	IdentifierOrCriteria *big.Int
	StartAmount          *big.Int
	EndAmount            *big.Int
}
type spConsiderationItem struct {
	ItemType             uint8
	Token                common.Address
	IdentifierOrCriteria *big.Int
	StartAmount          *big.Int
	EndAmount            *big.Int
	Recipient            common.Address
}
type spOrderParameters struct {
	Offerer                         common.Address
	Zone                            common.Address
	Offer                           []spOfferItem
	Consideration                   []spConsiderationItem
	OrderType                       uint8
	StartTime                       *big.Int
	EndTime                         *big.Int
	ZoneHash                        [32]byte
	Salt                            *big.Int
	ConduitKey                      [32]byte
	TotalOriginalConsiderationItems *big.Int
}
type spAdvancedOrder struct {
	Parameters  spOrderParameters
	Numerator   *big.Int
	Denominator *big.Int
	Signature   []byte
	ExtraData   []byte
}
type spCriteriaResolver struct {
	OrderIndex    *big.Int
	Side          uint8
	Index         *big.Int
	Identifier    *big.Int
	CriteriaProof [][32]byte
}
type spFulfillmentComponent struct {
	OrderIndex *big.Int
	ItemIndex  *big.Int
}
type spFulfillment struct {
	OfferComponents         []spFulfillmentComponent
	ConsiderationComponents []spFulfillmentComponent
}

// --- OpenSea input_data JSON shapes ---

type osInputData struct {
	Orders            []osAdvancedOrder    `json:"orders"`
	CriteriaResolvers []osCriteriaResolver `json:"criteriaResolvers"`
	Fulfillments      []osFulfillment      `json:"fulfillments"`
	Recipient         string               `json:"recipient"`
}
type osAdvancedOrder struct {
	Parameters  osOrderParams `json:"parameters"`
	Numerator   json.Number   `json:"numerator"`
	Denominator json.Number   `json:"denominator"`
	Signature   string        `json:"signature"`
	ExtraData   string        `json:"extraData"`
}
type osOrderParams struct {
	Offerer                         string         `json:"offerer"`
	Zone                            string         `json:"zone"`
	Offer                           []osOfferItem  `json:"offer"`
	Consideration                   []osConsItem   `json:"consideration"`
	OrderType                       uint8          `json:"orderType"`
	StartTime                       string         `json:"startTime"`
	EndTime                         string         `json:"endTime"`
	ZoneHash                        string         `json:"zoneHash"`
	Salt                            string         `json:"salt"`
	ConduitKey                      string         `json:"conduitKey"`
	TotalOriginalConsiderationItems json.Number    `json:"totalOriginalConsiderationItems"`
}
type osOfferItem struct {
	ItemType             uint8  `json:"itemType"`
	Token                string `json:"token"`
	IdentifierOrCriteria string `json:"identifierOrCriteria"`
	StartAmount          string `json:"startAmount"`
	EndAmount            string `json:"endAmount"`
}
type osConsItem struct {
	ItemType             uint8  `json:"itemType"`
	Token                string `json:"token"`
	IdentifierOrCriteria string `json:"identifierOrCriteria"`
	StartAmount          string `json:"startAmount"`
	EndAmount            string `json:"endAmount"`
	Recipient            string `json:"recipient"`
}
type osCriteriaResolver struct {
	OrderIndex    string   `json:"orderIndex"`
	Side          uint8    `json:"side"`
	Index         string   `json:"index"`
	Identifier    string   `json:"identifier"`
	CriteriaProof []string `json:"criteriaProof"`
}
type osFulfillment struct {
	OfferComponents         []osFulfillmentComp `json:"offerComponents"`
	ConsiderationComponents []osFulfillmentComp `json:"considerationComponents"`
}
type osFulfillmentComp struct {
	OrderIndex string `json:"orderIndex"`
	ItemIndex  string `json:"itemIndex"`
}

// BuildAcceptCalldata re-encodes OpenSea's fulfillment input_data (matchAdvancedOrders)
// into Seaport calldata. Returns the calldata; value is 0 (offers pay in WETH).
func BuildAcceptCalldata(inputData json.RawMessage) ([]byte, error) {
	var in osInputData
	if err := json.Unmarshal(inputData, &in); err != nil {
		return nil, fmt.Errorf("parse fulfillment: %w", err)
	}
	if len(in.Orders) == 0 {
		return nil, fmt.Errorf("no orders in fulfillment data")
	}
	orders := make([]spAdvancedOrder, len(in.Orders))
	for i, o := range in.Orders {
		orders[i] = spAdvancedOrder{
			Parameters:  mapParams(o.Parameters),
			Numerator:   numBig(o.Numerator),
			Denominator: numBig(o.Denominator),
			Signature:   common.FromHex(o.Signature),
			ExtraData:   common.FromHex(o.ExtraData),
		}
	}
	resolvers := make([]spCriteriaResolver, len(in.CriteriaResolvers))
	for i, c := range in.CriteriaResolvers {
		proof := make([][32]byte, len(c.CriteriaProof))
		for j, p := range c.CriteriaProof {
			proof[j] = toB32(p)
		}
		resolvers[i] = spCriteriaResolver{
			OrderIndex: decBig(c.OrderIndex), Side: c.Side, Index: decBig(c.Index),
			Identifier: decBig(c.Identifier), CriteriaProof: proof,
		}
	}
	fulfillments := make([]spFulfillment, len(in.Fulfillments))
	for i, f := range in.Fulfillments {
		fulfillments[i] = spFulfillment{
			OfferComponents:         mapComps(f.OfferComponents),
			ConsiderationComponents: mapComps(f.ConsiderationComponents),
		}
	}
	return matchAdvancedOrdersABI.Pack("matchAdvancedOrders", orders, resolvers, fulfillments, common.HexToAddress(in.Recipient))
}

func mapParams(p osOrderParams) spOrderParameters {
	offer := make([]spOfferItem, len(p.Offer))
	for i, o := range p.Offer {
		offer[i] = spOfferItem{ItemType: o.ItemType, Token: common.HexToAddress(o.Token), IdentifierOrCriteria: decBig(o.IdentifierOrCriteria), StartAmount: decBig(o.StartAmount), EndAmount: decBig(o.EndAmount)}
	}
	cons := make([]spConsiderationItem, len(p.Consideration))
	for i, c := range p.Consideration {
		cons[i] = spConsiderationItem{ItemType: c.ItemType, Token: common.HexToAddress(c.Token), IdentifierOrCriteria: decBig(c.IdentifierOrCriteria), StartAmount: decBig(c.StartAmount), EndAmount: decBig(c.EndAmount), Recipient: common.HexToAddress(c.Recipient)}
	}
	return spOrderParameters{
		Offerer: common.HexToAddress(p.Offerer), Zone: common.HexToAddress(p.Zone),
		Offer: offer, Consideration: cons, OrderType: p.OrderType,
		StartTime: decBig(p.StartTime), EndTime: decBig(p.EndTime), ZoneHash: toB32(p.ZoneHash),
		Salt: decBig(p.Salt), ConduitKey: toB32(p.ConduitKey), TotalOriginalConsiderationItems: numBig(p.TotalOriginalConsiderationItems),
	}
}

func mapComps(cs []osFulfillmentComp) []spFulfillmentComponent {
	out := make([]spFulfillmentComponent, len(cs))
	for i, c := range cs {
		out[i] = spFulfillmentComponent{OrderIndex: decBig(c.OrderIndex), ItemIndex: decBig(c.ItemIndex)}
	}
	return out
}

func decBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(strings.TrimSpace(s), 10)
	if !ok {
		return big.NewInt(0)
	}
	return v
}
func numBig(n json.Number) *big.Int {
	if n == "" {
		return big.NewInt(0)
	}
	return decBig(n.String())
}
func toB32(s string) [32]byte {
	var b [32]byte
	copy(b[:], common.FromHex(s))
	return b
}
