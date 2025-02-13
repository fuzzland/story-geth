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
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"net"
	"sync"
	"time"

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
	server           *grpc.Server
	bundles          []Bundle
	validatedBundles []Bundle
	mu               sync.RWMutex
	quit             chan struct{}

	node       *node.Node
	backend    Backend
	blockchain *core.BlockChain
}

type Bundle struct {
	Hash         common.Hash
	Transactions []types.Transaction
	MaxTimestamp uint64
	MinTimestamp uint64
}

// NewService creates a new bundle service
func NewService(node *node.Node, backend Backend) *Service {
	return &Service{
		server:           grpc.NewServer(),
		bundles:          make([]Bundle, 0),
		validatedBundles: make([]Bundle, 0),
		node:             node,
		backend:          backend,
		quit:             make(chan struct{}),
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
func (s *Service) AddBundle(bundle Bundle) error {
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

// processBundles processes all pending bundles
func (s *Service) processBundles() {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentBlock := s.backend.CurrentBlock()
	if currentBlock == nil {
		return
	}

	currentTime := uint64(time.Now().Unix())

	var validBundles []Bundle
	var remainingBundles []Bundle

	for _, bundle := range s.bundles {
		// Check timestamp constraints
		if bundle.MinTimestamp > currentTime || bundle.MaxTimestamp < currentTime {
			log.Debug("Dropping out-of-time bundle", "hash", bundle.Hash)
			continue
		}

		// Simulate bundle
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.SimulateBundle(ctx, &bundle)
		cancel()

		if err != nil {
			log.Debug("Bundle simulation failed", "hash", bundle.Hash, "error", err)
			continue
		}

		// If all checks pass, add to valid bundles
		validBundles = append(validBundles, bundle)
		remainingBundles = append(remainingBundles, bundle)
	}

	s.bundles = remainingBundles
	s.validatedBundles = validBundles
}

// GetValidatedBundles returns the currently validated bundles
func (s *Service) GetValidatedBundles() []Bundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Bundle{}, s.validatedBundles...)
}

// GetBundles implements the GetBundles RPC method
func (s *Service) GetBundles(ctx context.Context, req *pb.GetBundlesRequest) (*pb.GetBundlesResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Convert validated bundles to protobuf format
	pbBundles := make([]*pb.Bundle, len(s.validatedBundles))
	for i, bundle := range s.validatedBundles {
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

func (s *Service) SimulateBundle(ctx context.Context, bundle *Bundle) error {
	state, header, err := s.backend.StateAndHeaderByNumber(ctx, rpc.PendingBlockNumber)
	if err != nil {
		return err
	}

	for _, tx := range bundle.Transactions {
		msg, err := core.TransactionToMessage(&tx, types.LatestSignerForChainID(s.blockchain.Config().ChainID), header.BaseFee)
		if err != nil {
			return err
		}

		vmConfig := vm.Config{}
		blockContext := core.NewEVMBlockContext(header, s.blockchain, nil)
		evm := s.backend.GetEVM(ctx, msg, state, header, &vmConfig, &blockContext)

		snapshot := state.Snapshot()
		_, err = core.ApplyMessage(evm, msg, new(core.GasPool).AddGas(msg.GasLimit))
		if err != nil {
			state.RevertToSnapshot(snapshot)
			return err
		}
	}

	return nil
}
