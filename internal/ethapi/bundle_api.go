// BundleAPI offers an API for accepting bundled transactions
package ethapi

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/bundle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"golang.org/x/crypto/sha3"
)

// BundleAPI offers an API for accepting bundled transactions
type BundleAPI struct {
	b      Backend
	signer types.Signer
}

// NewBundleAPI creates a new Tx Bundle API instance.
func NewBundleAPI(b Backend) *BundleAPI {
	return &BundleAPI{b: b, signer: types.LatestSignerForChainID(b.ChainConfig().ChainID)}
}

// CallBundleArgs represents the arguments for a call.
type SendBundleArgs struct {
	Txs          []hexutil.Bytes `json:"txs"`
	MinTimestamp *uint64         `json:"minTimestamp,omitempty"`
	MaxTimestamp *uint64         `json:"maxTimestamp,omitempty"`
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
	senders := make([]common.Address, 0, len(args.Txs))

	for i, txBytes := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(txBytes); err != nil {
			return common.Hash{}, fmt.Errorf("transaction %d: %v", i, err)
		}
		txs[i] = *tx

		sender, err := types.Sender(s.signer, tx)
		if err != nil {
			return common.Hash{}, fmt.Errorf("transaction %d: invalid signature %v", i, err)
		}

		if slices.Contains(senders, sender) {
			continue
		}

		senders = append(senders, sender)
	}

	bundle := bundle.Bundle{
		Hash:         args.Hash(),
		Senders:      senders,
		Transactions: txs,
		MinTimestamp: 0,
		MaxTimestamp: ^uint64(0),
	}

	if args.MinTimestamp != nil {
		bundle.MinTimestamp = *args.MinTimestamp
	}

	if args.MaxTimestamp != nil {
		bundle.MaxTimestamp = uint64(time.Now().Unix() + 300) // 5 minutes = 300 seconds
	}

	bundleService := s.b.BundleService()

	ctx, _ = context.WithTimeout(ctx, 10*time.Second)

	_, _, err := bundleService.SimulateBundle(ctx, bundle.Transactions, nil)
	if err != nil {
		return common.Hash{}, err
	}

	if err := s.b.BundleService().AddBundle(&bundle); err != nil {
		return common.Hash{}, err
	}

	return bundle.Hash, nil
}

// SendRawTransaction will add the signed transaction to the transaction pool.
// The sender is responsible for signing the transaction and using the correct nonce.
func (s *BundleAPI) SendRawTransaction(ctx context.Context, input hexutil.Bytes) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return common.Hash{}, err
	}
	return s.SendBundle(ctx, SendBundleArgs{Txs: []hexutil.Bytes{input}})
}
