package appmanager

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"

	"cosmossdk.io/server/v2/stf"

	"cosmossdk.io/server/v2/core/appmanager"
	"cosmossdk.io/server/v2/core/store"
	"cosmossdk.io/server/v2/core/transaction"
)

type Tx struct {
	Tx         []byte // transaction bytes
	Identifier string // transaction Identifier
}

type TxPool interface {
	GetTxs(context.Context, uint32) ([]Tx, error)
}

type PrepareHandler func(ctx context.Context, txs []Tx) ([]Tx, []store.ChangeSet, error)

type AppManagerBuilder[T transaction.Tx] struct {
	InitGenesis map[string]func(ctx context.Context, moduleGenesisBytes []byte) error
}

func (a *AppManagerBuilder[T]) RegisterInitGenesis(moduleName string, genesisFunc func(ctx context.Context, moduleGenesisBytes []byte) error) {
	a.InitGenesis[moduleName] = genesisFunc
}

func (a *AppManagerBuilder[T]) RegisterHandler(moduleName, handlerName string, handler stf.MsgHandler) {
	panic("...")
}

type MsgSetKVPairs struct {
	Pairs []store.ChangeSet
}

func (a *AppManagerBuilder[T]) Build() *AppManager[T] {
	genesis := func(ctx context.Context, genesisBytes []byte) error {
		genesisMap := map[string][]byte{} // module=> genesis bytes
		for module, genesisFunc := range a.InitGenesis {
			err := genesisFunc(ctx, genesisMap[module])
			if err != nil {
				return fmt.Errorf("failed to init genesis on module: %s", module)
			}
		}
		return nil
	}
	return &AppManager[T]{initGenesis: genesis}
}

// AppManager is a coordinator for all things related to an application
type AppManager[T transaction.Tx] struct {
	// configs
	checkTxGasLimit    uint64
	queryGasLimit      uint64
	simulationGasLimit uint64
	// configs - end

	db store.Store

	txpool TxPool

	lastBlockHeight *atomic.Uint64

	initGenesis func(ctx context.Context, genesisBytes []byte) error

	stf *stf.STF[T]

	cachedState         []store.ChangeSet
	cachedTx            []Tx
	cachedBlockResponse *appmanager.BlockResponse
}

// BuildBlock builds a block when requested by consensus. It will take in a list of transactions and return a list of transactions
func (a AppManager[T]) BuildBlock(ctx context.Context, txs []Tx, totalSize uint32) ([]Tx, error) {

	txs, err := a.txpool.GetTxs(ctx, totalSize)
	if err != nil {
		return nil, err
	}

	// run txs through handler
	bsr, changeSets, err := a.PrepareBlock(ctx, txs)
	if err != nil {
		return nil, err
	}

	// cache the changes and txs to avoid execution later on
	if changeSets != nil && bsr != nil {
		a.cachedState = changeSets
		a.cachedBlockResponse = bsr
		a.cachedTx = txs
		return txs, nil
	}

	return txs, nil
}

func (a AppManager[T]) DeliverBlock(ctx context.Context, block appmanager.BlockRequest) (*appmanager.BlockResponse, Hash, error) {
	currentState, err := a.db.NewStateAt(block.Height)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create new state for height %d: %w", block.Height, err)
	}

	// if we cached values, avoid rexecuting
	if a.cachedState != nil && a.cachedTx != nil {
		diff := false
		for _, txs := range a.cachedTx {
			// compare txs to make sure they are whats in the cache, if not do normal execution
			for _, tx := range block.Txs {
				if !bytes.Equal(txs.Tx, tx) {
					// if txs dont match break and continue with normal execution
					// this means that a tx was added to the block which we did not optimistically execute
					diff = true
					break
				}
			}
		}

		if !diff {
			stateRoot, err := a.db.CommitState(a.cachedState)
			if err != nil {
				return nil, nil, fmt.Errorf("commit failed: %w", err)
			}
			return a.cachedBlockResponse, stateRoot, nil
		}
	}

	blockResponse, newState, err := a.stf.DeliverBlock(ctx, block, currentState)
	if err != nil {
		return nil, nil, fmt.Errorf("block delivery failed: %w", err)
	}

	// apply new state to store
	newStateChanges, err := newState.ChangeSets()
	if err != nil {
		return nil, nil, fmt.Errorf("change set: %w", err)
	}

	stateRoot, err := a.db.CommitState(newStateChanges)
	if err != nil {
		return nil, nil, fmt.Errorf("commit failed: %w", err)
	}
	// update last stored block
	a.lastBlockHeight.Store(block.Height)
	return blockResponse, stateRoot, nil
}

func (a AppManager[T]) Simulate(ctx context.Context, tx []byte) (appmanager.TxResult, error) {
	state, err := a.getLatestState(ctx)
	if err != nil {
		return appmanager.TxResult{}, err
	}
	result := a.stf.Simulate(ctx, state, a.simulationGasLimit, tx)
	return result, nil
}

func (a AppManager[T]) Query(ctx context.Context, request Type) (response Type, err error) {
	queryState, err := a.getLatestState(ctx)
	if err != nil {
		return nil, err
	}
	return a.stf.Query(ctx, queryState, a.queryGasLimit, request)
}

// getLatestState provides a readonly view of the state of the last committed block.
func (a AppManager[T]) getLatestState(_ context.Context) (store.ReadonlyState, error) {
	lastBlock := a.lastBlockHeight.Load()
	lastBlockState, err := a.db.ReadonlyStateAt(lastBlock)
	if err != nil {
		return nil, err
	}
	return lastBlockState, nil
}
