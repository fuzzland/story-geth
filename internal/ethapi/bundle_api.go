// BundleAPI offers an API for accepting bundled transactions
package ethapi

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/bundle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/crypto/sha3"
)

// BundleAPI offers an API for accepting bundled transactions
type BundleAPI struct {
	b Backend
}

// NewBundleAPI creates a new Tx Bundle API instance.
func NewBundleAPI(b Backend) *BundleAPI {
	return &BundleAPI{b}
}

// CallBundleArgs represents the arguments for a call.
type SendBundleArgs struct {
	Txs          []hexutil.Bytes  `json:"txs"`
	BlockNumber  *rpc.BlockNumber `json:"blockNumber,omitempty"`
	MinTimestamp *uint64          `json:"minTimestamp,omitempty"`
	MaxTimestamp *uint64          `json:"maxTimestamp,omitempty"`
}

func (a *SendBundleArgs) Hash() common.Hash {
	hasher := sha3.NewLegacyKeccak256()

	for _, tx := range a.Txs {
		h := sha3.NewLegacyKeccak256().Sum(tx)
		hasher.Write(h)
	}

	return common.Hash(hasher.Sum(nil))
}

func (s *BundleAPI) SendBundle(ctx context.Context, args SendBundleArgs) (common.Hash, error) {
	txs := make([]types.Transaction, len(args.Txs))
	for i, txBytes := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(txBytes); err != nil {
			return common.Hash{}, fmt.Errorf("transaction %d: %v", i, err)
		}
		txs[i] = *tx
	}

	blockNumber := rpc.LatestBlockNumber
	if args.BlockNumber != nil {
		blockNumber = *args.BlockNumber
	}

	bundle := bundle.Bundle{
		Hash:         args.Hash(),
		Transactions: txs,
		BlockNumber:  blockNumber,
		MinTimestamp: 0,
		MaxTimestamp: ^uint64(0),
	}

	if args.MinTimestamp != nil {
		bundle.MinTimestamp = *args.MinTimestamp
	}
	if args.MaxTimestamp != nil {
		bundle.MaxTimestamp = *args.MaxTimestamp
	}

	if err := s.b.BundleService().AddBundle(bundle); err != nil {
		return common.Hash{}, err
	}

	return bundle.Hash, nil
}
