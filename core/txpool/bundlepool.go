package txpool

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	pb "github.com/ethereum/go-ethereum/proto/bundle"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type BundlePool interface {
	PendingBundles(filter PendingFilter) [][]*types.Transaction
}

type evictableBundle struct {
	bundle   []*types.Transaction
	deadline uint64
}

type StoryFlowBundlePool struct {
	pool []*evictableBundle

	client pb.BundleServiceClient
	conn   *grpc.ClientConn
	signer ecdsa.PrivateKey
}

func (p *StoryFlowBundlePool) start() error {
	conn, err := grpc.NewClient(
		"orderflow.storyflow.xyz:443",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	p.conn = conn
	p.client = pb.NewBundleServiceClient(conn)

	go p.pullBundles()
	return nil
}

func (p *StoryFlowBundlePool) pullBundles() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Generate random hash to sign
		randomBytes := make([]byte, 32)
		rand.Read(randomBytes)
		hash := common.BytesToHash(randomBytes)

		// Sign the hash
		signature, err := ecdsa.SignASN1(rand.Reader, &p.signer, hash.Bytes())
		if err != nil {
			continue
		}

		// Make gRPC request with signature and hash in header
		ctx := context.Background()
		ctx = metadata.AppendToOutgoingContext(ctx,
			"signature", hexutil.Encode(signature),
			"hash", hash.Hex(),
		)

		resp, err := p.client.GetBundles(ctx, &pb.GetBundlesRequest{})
		if err != nil {
			continue
		}

		// Convert protobuf bundles to types.Transaction
		var bundles []*evictableBundle
		for _, bundle := range resp.Bundles {
			var txs []*types.Transaction
			for _, txBytes := range bundle.Transactions {
				tx := new(types.Transaction)
				if err := tx.UnmarshalBinary(txBytes); err != nil {
					continue
				}
				txs = append(txs, tx)
			}
			bundles = append(bundles, &evictableBundle{
				bundle:   txs,
				deadline: bundle.Deadline,
			})
		}
		log.Info("Pulled bundles", "count", len(bundles))
		p.pool = bundles
	}
}

func (p *StoryFlowBundlePool) stop() error {
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

func (p *StoryFlowBundlePool) PendingBundles(filter PendingFilter) [][]*types.Transaction {
	var (
		minTipBig  *big.Int
		baseFeeBig *big.Int
	)
	if filter.MinTip != nil {
		minTipBig = filter.MinTip.ToBig()
	}
	if filter.BaseFee != nil {
		baseFeeBig = filter.BaseFee.ToBig()
	}

	var pending [][]*types.Transaction

	now := uint64(time.Now().Unix())
	evictionIds := []int{}
	for i, bundle := range p.pool {
		if bundle.deadline < now {
			evictionIds = append(evictionIds, i)
		}
		var isFiltered bool
		for _, tx := range bundle.bundle {
			if tx.EffectiveGasTipIntCmp(minTipBig, baseFeeBig) < 0 {
				isFiltered = true
				break
			}
		}
		if !isFiltered {
			pending = append(pending, bundle.bundle)
		}
	}
	for i := len(evictionIds) - 1; i >= 0; i-- {
		p.pool = append(p.pool[:evictionIds[i]], p.pool[evictionIds[i]+1:]...)
	}
	return pending
}

func NewStoryFlowBundlePool(key *ecdsa.PrivateKey) BundlePool {
	sp := &StoryFlowBundlePool{}
	sp.signer = *key
	sp.start()
	return sp
}
