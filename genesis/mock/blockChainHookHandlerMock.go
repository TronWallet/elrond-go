package mock

import (
	"math/big"

	"github.com/ElrondNetwork/elrond-go/data"
	"github.com/ElrondNetwork/elrond-go/data/state"
	"github.com/ElrondNetwork/elrond-go/process"
)

// BlockChainHookHandlerMock -
type BlockChainHookHandlerMock struct {
	AddTempAccountCalled    func(address []byte, balance *big.Int, nonce uint64)
	CleanTempAccountsCalled func()
	TempAccountCalled       func(address []byte) state.AccountHandler
	SetCurrentHeaderCalled  func(hdr data.HeaderHandler)
	NewAddressCalled        func(creatorAddress []byte, creatorNonce uint64, vmType []byte) ([]byte, error)
}

// GetBuiltInFunctions -
func (e *BlockChainHookHandlerMock) GetBuiltInFunctions() process.BuiltInFunctionContainer {
	return nil
}

// AddTempAccount -
func (e *BlockChainHookHandlerMock) AddTempAccount(address []byte, balance *big.Int, nonce uint64) {
	if e.AddTempAccountCalled != nil {
		e.AddTempAccountCalled(address, balance, nonce)
	}
}

// CleanTempAccounts -
func (e *BlockChainHookHandlerMock) CleanTempAccounts() {
	if e.CleanTempAccountsCalled != nil {
		e.CleanTempAccountsCalled()
	}
}

// TempAccount -
func (e *BlockChainHookHandlerMock) TempAccount(address []byte) state.AccountHandler {
	if e.TempAccountCalled != nil {
		return e.TempAccountCalled(address)
	}
	return nil
}

// IsInterfaceNil -
func (e *BlockChainHookHandlerMock) IsInterfaceNil() bool {
	return e == nil
}

// SetCurrentHeader -
func (e *BlockChainHookHandlerMock) SetCurrentHeader(hdr data.HeaderHandler) {
	if e.SetCurrentHeaderCalled != nil {
		e.SetCurrentHeaderCalled(hdr)
	}
}

// NewAddress -
func (e *BlockChainHookHandlerMock) NewAddress(creatorAddress []byte, creatorNonce uint64, vmType []byte) ([]byte, error) {
	if e.NewAddressCalled != nil {
		return e.NewAddressCalled(creatorAddress, creatorNonce, vmType)
	}

	return make([]byte, 0), nil
}
