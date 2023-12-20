package appmanager

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/cosmos/cosmos-sdk/serverv2/core/appmanager"
	"github.com/cosmos/cosmos-sdk/serverv2/core/transaction"
)

type AppManagerBuilder[T transaction.Tx] struct {
	InitGenesis map[string]func(ctx context.Context, moduleGenesisBytes []byte) error
}

func (a *AppManagerBuilder[T]) RegisterInitGenesis(moduleName string, genesisFunc func(ctx context.Context, moduleGenesisBytes []byte) error) {
	a.InitGenesis[moduleName] = genesisFunc
}

func (a *AppManagerBuilder[T]) RegisterHandler(moduleName, handlerName string, handler MsgHandler) {
	panic("...")
}

type MsgSetKVPairs struct {
	Pairs []ChangeSet
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

type AppManager[T transaction.Tx] struct {
	// configs
	checkTxGasLimit uint64
	queryGasLimit   uint64
	// configs - end

	db Store

	lastBlockHeight *atomic.Uint64

	initGenesis func(ctx context.Context, genesisBytes []byte) error

	stf *STFAppManager[T]
}

func (a AppManager[T]) DeliverBlock(ctx context.Context, block appmanager.BlockRequest) (*appmanager.BlockResponse, Hash, error) {
	currentState, err := a.db.NewBlockWithVersion(block.Height)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create new state for height %d: %w", block.Height, err)
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
	stateRoot, err := a.db.CommitChanges(newStateChanges)
	if err != nil {
		return nil, nil, fmt.Errorf("commit failed: %w", err)
	}
	// update last stored block
	a.lastBlockHeight.Store(block.Height)
	return blockResponse, stateRoot, nil
}

func (a AppManager[T]) Query(ctx context.Context, request Type) (response Type, err error) {
	queryState, err := a.getLatestState(ctx)
	if err != nil {
		return nil, err
	}
	queryCtx := a.stf.MakeContext(ctx, nil, queryState, a.queryGasLimit)
	return a.stf.handleQuery(queryCtx, request)
}

// getLatestState provides a readonly view of the state of the last committed block.
func (a AppManager[T]) getLatestState(_ context.Context) (BranchStore, error) {
	lastBlock := a.lastBlockHeight.Load()
	lastBlockStore, err := a.db.ReadonlyWithVersion(lastBlock)
	if err != nil {
		return nil, err
	}
	return a.stf.branch(lastBlockStore), nil
}