package spos_test

import (
	"testing"

	"github.com/ElrondNetwork/elrond-go/consensus/spos"
	"github.com/ElrondNetwork/elrond-go/consensus/spos/bls"
	"github.com/stretchr/testify/assert"
)

func TestRoundThreshold_NewThresholdShouldWork(t *testing.T) {
	t.Parallel()

	rthr := spos.NewRoundThreshold()

	assert.NotNil(t, rthr)
}

func TestRoundThreshold_SetThresholdShouldWork(t *testing.T) {
	t.Parallel()

	rthr := spos.NewRoundThreshold()

	rthr.SetThreshold(bls.SrBlock, 1)
	rthr.SetThreshold(bls.SrSignature, 5)

	assert.Equal(t, 5, rthr.Threshold(bls.SrSignature))
}
