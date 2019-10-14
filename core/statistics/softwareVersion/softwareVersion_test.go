package softwareVersion

import (
	"testing"

	"github.com/ElrondNetwork/elrond-go/consensus/mock"
	"github.com/ElrondNetwork/elrond-go/core/appStatusPolling"
	"github.com/stretchr/testify/assert"
)

func TestNewSoftwareVersionChecker_NilStatusHandlerShouldErr(t *testing.T) {
	t.Parallel()

	softwareChecker, err := NewSoftwareVersionChecker(nil)

	assert.Nil(t, softwareChecker)
	assert.Equal(t, appStatusPolling.ErrNilAppStatusHandler, err)
}

func TestNewSoftwareVersionChecker(t *testing.T) {
	t.Parallel()

	statusHandler := &mock.AppStatusHandlerMock{}
	softwareChecker, err := NewSoftwareVersionChecker(statusHandler)

	assert.Nil(t, err)
	assert.NotNil(t, softwareChecker)
}

func TestCheckSoftwareVersion_Read(t *testing.T) {
	t.Parallel()

	tag, err := readJSONFromUrl(stableTagLocation)

	assert.Nil(t, err)
	assert.NotEqual(t, "", tag)
}
