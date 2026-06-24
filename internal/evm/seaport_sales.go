package evm

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// erc721TransferTopic is topic0 for Transfer(address,address,uint256). An ERC-721 mint is a
// Transfer FROM the zero address; ERC-721/1155 logs carry 4 topics (sig+from+to+tokenId),
// which distinguishes them from ERC-20 transfers (3 topics).
var erc721TransferTopic = gethcrypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// MintInfo locates where a wallet minted an NFT on-chain.
type MintInfo struct {
	TxHash common.Hash
	Block  uint64
}

// FindMint looks for the on-chain mint of (contract, tokenID) TO wallet — an ERC-721
// Transfer from the zero address — within [from,to]. ok=false means the wallet did not mint
// it (it was bought, or the mint is outside the window). The query is fully indexed
// (contract + from=0 + to=wallet + tokenId), so the node returns at most one log.
func FindMint(ctx context.Context, c *ethclient.Client, contract common.Address, tokenID, from, to *big.Int, wallet common.Address) (MintInfo, bool) {
	q := ethereum.FilterQuery{
		FromBlock: from, ToBlock: to,
		Addresses: []common.Address{contract},
		Topics: [][]common.Hash{
			{erc721TransferTopic},
			{common.Hash{}},                       // from = zero address (a mint)
			{common.BytesToHash(wallet.Bytes())},  // to = our wallet
			{common.BytesToHash(tokenID.Bytes())}, // tokenId
		},
	}
	logs, err := c.FilterLogs(ctx, q)
	if err != nil || len(logs) == 0 {
		return MintInfo{}, false
	}
	return MintInfo{TxHash: logs[0].TxHash, Block: logs[0].BlockNumber}, true
}

// MintCost reads what `wallet` paid to mint, from the mint tx: (tx value + gas) split across
// the NFTs minted to the wallet in that same tx (a batch mint divides the cost evenly).
func MintCost(ctx context.Context, c *ethclient.Client, txHash common.Hash, wallet common.Address) (string, error) {
	tx, _, err := c.TransactionByHash(ctx, txHash)
	if err != nil {
		return "0", err
	}
	rcpt, err := c.TransactionReceipt(ctx, txHash)
	if err != nil {
		return "0", err
	}
	total := new(big.Int).Set(tx.Value())
	if rcpt.EffectiveGasPrice != nil {
		total.Add(total, new(big.Int).Mul(new(big.Int).SetUint64(rcpt.GasUsed), rcpt.EffectiveGasPrice))
	}
	walletTopic := common.BytesToHash(wallet.Bytes())
	n := int64(0)
	for _, lg := range rcpt.Logs {
		if len(lg.Topics) == 4 && lg.Topics[0] == erc721TransferTopic && lg.Topics[1] == (common.Hash{}) && lg.Topics[2] == walletTopic {
			n++
		}
	}
	if n < 1 {
		n = 1
	}
	return new(big.Int).Div(total, big.NewInt(n)).String(), nil
}

// orderFulfilledABI decodes Seaport's OrderFulfilled event. offerer + zone are indexed
// (carried in log topics); the rest is in log data. Verified against a live mainnet sale.
var orderFulfilledABI = mustABI(`[{"type":"event","name":"OrderFulfilled","inputs":[
 {"name":"orderHash","type":"bytes32","indexed":false},
 {"name":"offerer","type":"address","indexed":true},
 {"name":"zone","type":"address","indexed":true},
 {"name":"recipient","type":"address","indexed":false},
 {"name":"offer","type":"tuple[]","indexed":false,"components":[{"name":"itemType","type":"uint8"},{"name":"token","type":"address"},{"name":"identifier","type":"uint256"},{"name":"amount","type":"uint256"}]},
 {"name":"consideration","type":"tuple[]","indexed":false,"components":[{"name":"itemType","type":"uint8"},{"name":"token","type":"address"},{"name":"identifier","type":"uint256"},{"name":"amount","type":"uint256"},{"name":"recipient","type":"address"}]}
]}]`)

type ofSpent struct {
	ItemType   uint8
	Token      common.Address
	Identifier *big.Int
	Amount     *big.Int
}
type ofReceived struct {
	ItemType   uint8
	Token      common.Address
	Identifier *big.Int
	Amount     *big.Int
	Recipient  common.Address
}

// OnchainSale is a confirmed Seaport sale where one of our wallets was the seller (the
// order's offerer): the NFT that left the wallet plus the exact net ETH/WETH proceeds it
// received, read straight from the event logs — no marketplace API involved.
type OnchainSale struct {
	TxHash      string
	Seller      string
	Contract    string
	TokenID     string
	ProceedsWei string
	Block       uint64
}

// OrderFulfilledTopic is the log topic0 for Seaport's OrderFulfilled event.
func OrderFulfilledTopic() common.Hash { return orderFulfilledABI.Events["OrderFulfilled"].ID }

// ScanSeaportSales reads Seaport 1.6 OrderFulfilled logs in [fromBlock, toBlock] where the
// offerer (seller) is one of `sellers`, returning each as a confirmed NFT sale with the net
// proceeds paid to that seller. The offerer filter is an indexed topic, so the node only
// returns our wallets' sales. A listing that sold off-app surfaces here; an offer WE
// accepted does not (there the buyer is the offerer) and is booked separately.
func ScanSeaportSales(ctx context.Context, c *ethclient.Client, sellers []common.Address, fromBlock, toBlock *big.Int) ([]OnchainSale, error) {
	if len(sellers) == 0 {
		return nil, nil
	}
	sellerTopics := make([]common.Hash, len(sellers))
	for i, s := range sellers {
		sellerTopics[i] = common.BytesToHash(s.Bytes())
	}
	q := ethereum.FilterQuery{
		FromBlock: fromBlock, ToBlock: toBlock,
		Addresses: []common.Address{common.HexToAddress(Seaport16)},
		Topics:    [][]common.Hash{{OrderFulfilledTopic()}, sellerTopics},
	}
	logs, err := c.FilterLogs(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []OnchainSale
	for _, lg := range logs {
		if len(lg.Topics) < 2 {
			continue
		}
		var d struct {
			OrderHash     [32]byte
			Recipient     common.Address
			Offer         []ofSpent
			Consideration []ofReceived
		}
		if err := orderFulfilledABI.UnpackIntoInterface(&d, "OrderFulfilled", lg.Data); err != nil {
			continue
		}
		offerer := common.BytesToAddress(lg.Topics[1].Bytes())
		// The NFT the seller gave up (ERC721 itemType 2 / ERC1155 itemType 3).
		var nft *ofSpent
		for i := range d.Offer {
			if d.Offer[i].ItemType == 2 || d.Offer[i].ItemType == 3 {
				nft = &d.Offer[i]
				break
			}
		}
		if nft == nil {
			continue // offerer wasn't selling an NFT in this order
		}
		// Net proceeds = ETH (itemType 0) / ERC-20 (itemType 1) consideration paid to the seller.
		proceeds := new(big.Int)
		for _, cs := range d.Consideration {
			if cs.Recipient == offerer && (cs.ItemType == 0 || cs.ItemType == 1) {
				proceeds.Add(proceeds, cs.Amount)
			}
		}
		out = append(out, OnchainSale{
			TxHash: lg.TxHash.Hex(), Seller: offerer.Hex(), Contract: nft.Token.Hex(),
			TokenID: nft.Identifier.String(), ProceedsWei: proceeds.String(), Block: lg.BlockNumber,
		})
	}
	return out, nil
}
