// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package bundle

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"

	"github.com/ethereum/go-ethereum/rpc"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	pb "github.com/ethereum/go-ethereum/proto/bundle"
	"google.golang.org/grpc"
)

type Backend interface {
	CurrentBlock() *types.Header
	StateAndHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*state.StateDB, *types.Header, error)
	StateAndHeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error)
	GetEVM(ctx context.Context, msg *core.Message, state *state.StateDB, header *types.Header, vmConfig *vm.Config, blockCtx *vm.BlockContext) *vm.EVM
}

// Service implements the bundle service gRPC server
type Service struct {
	pb.UnimplementedBundleServiceServer
	server *grpc.Server

	mu   sync.RWMutex
	quit chan struct{}

	bundles       []*Bundle
	sortedBundles []*Bundle

	node    *node.Node
	backend Backend
	chain   *core.BlockChain
	signer  types.Signer
}

type Bundle struct {
	Hash         common.Hash
	Senders      []common.Address
	Transactions []types.Transaction
	MaxTimestamp uint64
	MinTimestamp uint64
}

// NewService creates a new bundle service
func NewService(node *node.Node, backend Backend, chain *core.BlockChain) *Service {

	return &Service{
		server: grpc.NewServer(),

		quit: make(chan struct{}),

		bundles:       make([]*Bundle, 0),
		sortedBundles: make([]*Bundle, 0),

		node:    node,
		backend: backend,
		chain:   chain,
		signer:  types.LatestSignerForChainID(chain.Config().ChainID),
	}
}

// Start starts the bundle service
func (s *Service) Start() error {
	// Register the gRPC service
	pb.RegisterBundleServiceServer(s.server, s)

	// Get relay server config
	cfg := s.node.Config().RelayServer
	if cfg.Host == "" {
		return fmt.Errorf("relay server host not configured")
	}

	// Start listening
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", addr, err)
	}

	// Start server in a goroutine
	go func() {
		log.Info("Starting bundle relay server", "addr", addr)
		if err := s.server.Serve(lis); err != nil {
			log.Error("Bundle relay server failed", "err", err)
		}
	}()

	// Start bundle simulation routine
	go s.simulationLoop()

	return nil
}

// Stop stops the bundle service
func (s *Service) Stop() error {
	close(s.quit)
	if s.server != nil {
		s.server.GracefulStop()
	}
	return nil
}

// AddBundle adds a new bundle to the service
func (s *Service) AddBundle(bundle *Bundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles = append(s.bundles, bundle)
	return nil
}

// simulationLoop continuously processes bundles
func (s *Service) simulationLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.quit:
			return
		case <-ticker.C:
			s.processBundles()
		}
	}
}

type bundleInfo struct {
	bundle         Bundle
	coinbasePayout uint256.Int
	primaryPayout  uint256.Int
}

// processBundles processes all pending bundles
func (s *Service) processBundles() {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentBlock := s.backend.CurrentBlock()
	if currentBlock == nil {
		return
	}

	currentTime := uint64(time.Now().Unix())

	var remainingBundles []*Bundle
	var validBundles []bundleInfo

	for _, bundle := range s.bundles {
		// Check timestamp constraints
		if bundle.MinTimestamp > currentTime || bundle.MaxTimestamp < currentTime {
			log.Debug("Dropping out-of-time bundle", "hash", bundle.Hash)
			continue
		}

		// Simulate bundle
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		coinbasePayout, primaryPayout, err := s.SimulateBundle(ctx, bundle.Transactions, &s.node.Config().RelayServer.PayoutAddress)
		cancel()

		if err != nil {
			log.Debug("Bundle simulation failed", "hash", bundle.Hash, "error", err)
			continue
		}

		// If all checks pass, add to valid bundles
		validBundles = append(validBundles, bundleInfo{
			bundle:         *bundle,
			coinbasePayout: *coinbasePayout,
			primaryPayout:  *primaryPayout,
		})
		remainingBundles = append(remainingBundles, bundle)
	}

	// Sorted bundles by primary payout and then coinbase payout, DESCENDING
	sort.Slice(validBundles, func(i, j int) bool {
		if validBundles[i].primaryPayout.Cmp(&validBundles[j].primaryPayout) == 0 {
			return validBundles[i].coinbasePayout.Cmp(&validBundles[j].coinbasePayout) > 0
		}

		return validBundles[i].primaryPayout.Cmp(&validBundles[j].primaryPayout) > 0
	})

	var sortedBundles []*Bundle
	var touchedSenders []common.Address

	for _, bundle := range validBundles {
		var accountConflict bool
		for _, sender := range bundle.bundle.Senders {
			if slices.Contains(touchedSenders, sender) {
				accountConflict = true
				break
			}
		}

		if accountConflict {
			continue
		}

		sortedBundles = append(sortedBundles, &bundle.bundle)
		touchedSenders = append(touchedSenders, bundle.bundle.Senders...)
	}

	s.bundles = remainingBundles
	s.sortedBundles = sortedBundles
}

// GetValidatedBundles returns the currently validated bundles
func (s *Service) GetValidatedBundles() []*Bundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sortedBundles
}

// GetBundles implements the GetBundles RPC method
func (s *Service) GetBundles(ctx context.Context, req *pb.GetBundlesRequest) (*pb.GetBundlesResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Convert validated bundles to protobuf format
	pbBundles := make([]*pb.Bundle, len(s.sortedBundles))
	for i, bundle := range s.sortedBundles {
		pbTxs := make([][]byte, len(bundle.Transactions))
		for j, tx := range bundle.Transactions {
			txBytes, err := tx.MarshalBinary()
			if err != nil {
				return nil, err
			}
			pbTxs[j] = txBytes
		}

		pbBundles[i] = &pb.Bundle{
			Transactions: pbTxs,
		}
	}

	return &pb.GetBundlesResponse{
		Bundles: pbBundles,
	}, nil
}

func (s *Service) SimulateBundle(ctx context.Context, txs []types.Transaction, primaryBeneficiary *common.Address) (*uint256.Int, *uint256.Int, error) {
	state, header, err := s.backend.StateAndHeaderByNumber(ctx, rpc.PendingBlockNumber)
	if err != nil {
		return nil, nil, err
	}

	vmConfig := vm.Config{}
	blockContext := core.NewEVMBlockContext(header, s.chain, nil)
	gp := new(core.GasPool).AddGas(header.GasLimit)

	primaryBalance := uint256.NewInt(0)
	coinbaseBalance := state.GetBalance(header.Coinbase)

	if primaryBalance != nil {
		primaryBalance = state.GetBalance(*primaryBeneficiary)
	}

	for _, tx := range txs {
		msg, err := core.TransactionToMessage(&tx, s.signer, header.BaseFee)
		if err != nil {
			return nil, nil, err
		}

		evm := s.backend.GetEVM(ctx, msg, state, header, &vmConfig, &blockContext)
		_, err = core.ApplyMessage(evm, msg, gp)
		if err != nil {
			return nil, nil, err
		}
	}

	coinbasePayout, overflow := uint256.NewInt(0).SubOverflow(state.GetBalance(header.Coinbase), coinbaseBalance)
	if overflow {
		return nil, nil, fmt.Errorf("coinbase payout overflow")
	}

	primaryPayout := uint256.NewInt(0)
	if primaryBalance != nil {
		primaryPayout, overflow = uint256.NewInt(0).SubOverflow(primaryBalance, coinbasePayout)
		if overflow {
			return nil, nil, fmt.Errorf("primary payout overflow")
		}
	}

	return coinbasePayout, primaryPayout, nil
}
