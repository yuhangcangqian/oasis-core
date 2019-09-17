package byzantine

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	tmpubsub "github.com/tendermint/tendermint/libs/pubsub"
	tmtypes "github.com/tendermint/tendermint/types"

	beacon "github.com/oasislabs/ekiden/go/beacon/api"
	"github.com/oasislabs/ekiden/go/common/cbor"
	"github.com/oasislabs/ekiden/go/common/identity"
	"github.com/oasislabs/ekiden/go/common/pubsub"
	epochtime "github.com/oasislabs/ekiden/go/epochtime/api"
	"github.com/oasislabs/ekiden/go/genesis"
	registry "github.com/oasislabs/ekiden/go/registry/api"
	scheduler "github.com/oasislabs/ekiden/go/scheduler/api"
	"github.com/oasislabs/ekiden/go/tendermint"
	beaconapp "github.com/oasislabs/ekiden/go/tendermint/apps/beacon"
	epochtime_mockapp "github.com/oasislabs/ekiden/go/tendermint/apps/epochtime_mock"
	keymanagerapp "github.com/oasislabs/ekiden/go/tendermint/apps/keymanager"
	registryapp "github.com/oasislabs/ekiden/go/tendermint/apps/registry"
	roothashapp "github.com/oasislabs/ekiden/go/tendermint/apps/roothash"
	schedulerapp "github.com/oasislabs/ekiden/go/tendermint/apps/scheduler"
	stakingapp "github.com/oasislabs/ekiden/go/tendermint/apps/staking"
	"github.com/oasislabs/ekiden/go/tendermint/service"
)

var _ epochtime.Backend = (*fakeTimeBackend)(nil)

// fakeTimeBackend is like TendermintBackend (of epochtime), but without
// any workers.
type fakeTimeBackend struct {
	service service.TendermintService

	useMockEpochTime bool
}

// GetEpoch implements epochtime Backend.
func (t *fakeTimeBackend) GetEpoch(ctx context.Context, height int64) (epochtime.EpochTime, error) {
	if height == 0 {
		panic("0 height not supported")
	}

	if t.useMockEpochTime {
		// Query the epochtime_mock Tendermint application.
		response, err := t.service.Query(epochtime_mockapp.QueryGetEpoch, nil, height)
		if err != nil {
			return 0, errors.Wrap(err, "epochtime: get block epoch query failed")
		}

		var data epochtime_mockapp.QueryGetEpochResponse
		if err := cbor.Unmarshal(response, &data); err != nil {
			return 0, errors.Wrap(err, "epochtime: get block epoch malformed response")
		}

		return data.Epoch, nil
	}

	// Use the the epoch interval that we have in E2E tests.
	// We could make this more flexible with command line flags in future work.
	return epochtime.EpochTime(height / 30), nil
}

// GetEpochBlock implements epochtime Backend.
func (*fakeTimeBackend) GetEpochBlock(ctx context.Context, epoch epochtime.EpochTime) (int64, error) {
	panic("GetEpochBlock not supported")
}

// WatchEpochs implements epochtime Backend.
func (*fakeTimeBackend) WatchEpochs() (<-chan epochtime.EpochTime, *pubsub.Subscription) {
	panic("WatchEpochs not supported")
}

type honestTendermint struct {
	service service.TendermintService
}

func newHonestTendermint() *honestTendermint {
	return &honestTendermint{}
}

func (ht *honestTendermint) start(id *identity.Identity, dataDir string, useMockEpochTime bool) error {
	if ht.service != nil {
		return errors.New("honest Tendermint service already started")
	}

	genesis, err := genesis.New()
	if err != nil {
		return errors.Wrap(err, "genesis New")
	}
	ht.service = tendermint.New(context.Background(), dataDir, id, genesis)

	if err := ht.service.ForceInitialize(); err != nil {
		return errors.Wrap(err, "honest Tendermint service ForceInitialize")
	}

	// Register honest mux apps.
	// This isn't very flexible. It's configured to match what we use in end-to-end tests.
	// And we do that mostly by hardcoding options. We could make this more flexible with command
	// line flags in future work.
	timeSource := &fakeTimeBackend{
		service:          ht.service,
		useMockEpochTime: useMockEpochTime,
	}
	if useMockEpochTime {
		if err := ht.service.RegisterApplication(epochtime_mockapp.New()); err != nil {
			return errors.Wrap(err, "honest Tendermint service RegisterApplication epochtime_mock")
		}
	}
	if err := ht.service.RegisterApplication(beaconapp.New(timeSource, &beacon.Config{
		DebugDeterministic: true,
	})); err != nil {
		return errors.Wrap(err, "honest Tendermint service RegisterApplication beacon")
	}
	if err := ht.service.RegisterApplication(stakingapp.New(nil)); err != nil {
		return errors.Wrap(err, "honest Tendermint service RegisterApplication staking")
	}
	if err := ht.service.RegisterApplication(registryapp.New(timeSource, &registry.Config{
		DebugAllowRuntimeRegistration: false,
		DebugBypassStake:              false,
	})); err != nil {
		return errors.Wrap(err, "honest Tendermint service RegisterApplication registry")
	}
	if err := ht.service.RegisterApplication(keymanagerapp.New(timeSource)); err != nil {
		return errors.Wrap(err, "honest Tendermint service RegisterApplication keymanager")
	}
	if err := ht.service.RegisterApplication(schedulerapp.New(timeSource, &scheduler.Config{
		DebugBypassStake: false,
	})); err != nil {
		return errors.Wrap(err, "honest Tendermint service RegisterApplication scheduler")
	}
	// storage has no registration
	if err := ht.service.RegisterApplication(roothashapp.New(context.Background(), timeSource, nil, 10*time.Second)); err != nil {
		return errors.Wrap(err, "honest Tendermint service RegisterApplication roothash")
	}

	if err := ht.service.Start(); err != nil {
		return errors.Wrap(err, "honest Tendermint service Start")
	}
	logger.Debug("honest Tendermint service waiting for Tendermint start")
	<-ht.service.Started()
	logger.Debug("honest Tendermint service waiting for Tendermint sync")
	<-ht.service.Synced()
	logger.Debug("honest Tendermint service sync done")

	return nil
}

func (ht honestTendermint) stop() error {
	if ht.service == nil {
		return errors.New("honest Tendermint service not started")
	}

	ht.service.Stop()
	logger.Debug("honest Tendermint service waiting for quit")
	<-ht.service.Quit()
	logger.Debug("honest Tendermint service quit done")
	ht.service = nil

	return nil
}

// tendermintUnsubscribeDrain drains an unbuffered subscription while unsubscribing.
func tendermintUnsubscribeDrain(svc service.TendermintService, subscriber string, query tmpubsub.Query, sub tmtypes.Subscription) error {
	go func() {
		for {
			select {
			case <-sub.Out():
			case <-sub.Cancelled():
				break
			}
		}
	}()
	if err := svc.Unsubscribe("script", query); err != nil {
		return errors.Wrap(err, "Tendermint Unsubscribe")
	}

	return nil
}

// tendermintBroadcastTxCommit is like Tendermint's own BroadcastTxCommit, but without
// the timeout system.
func tendermintBroadcastTxCommit(svc service.TendermintService, tag byte, tx interface{}) error {
	q := tmtypes.EventQueryTxFor(svc.MarshalTx(tag, tx))
	deliverTxSub, err := svc.Subscribe("script", q)
	if err != nil {
		return errors.Wrap(err, "Tendermint Subscribe")
	}
	defer func() {
		if err1 := tendermintUnsubscribeDrain(svc, "script", q, deliverTxSub); err1 != nil {
			panic(fmt.Sprintf("Tendermint unsubscribe drain: %+v", err1))
		}
	}()

	if err = svc.BroadcastTx(tag, tx); err != nil {
		return errors.Wrap(err, "Tendermint BroadcastTx")
	}

	select {
	case <-deliverTxSub.Out():
		return nil
	case <-deliverTxSub.Cancelled():
		return errors.Wrap(deliverTxSub.Err(), "deliverTx subscription cancelled")
	}
}
