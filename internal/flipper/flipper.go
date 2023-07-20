package flipper

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/metal-toolbox/cookieflipper/internal/store"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.hollow.sh/toolbox/events"
	"go.hollow.sh/toolbox/events/registry"
)

const (
	pkgName = "internal/worker"
)

var (
	fetchEventsInterval = 10 * time.Second

	// conditionTimeout defines the time after which the condition execution will be cancelled.
	conditionTimeout = 180 * time.Minute

	errConditionDeserialize = errors.New("unable to deserialize condition")
)

// Flipper holds attributes to run a cookie flipper instance
type Flipper struct {
	stream            events.Stream
	store             store.Repository
	syncWG            *sync.WaitGroup
	logger            *logrus.Logger
	name              string
	id                registry.ControllerID // assigned when this worker registers itself
	facilityCode      string
	concurrency       int
	dispatched        int32
	dryrun            bool
	faultInjection    bool
	useStatusKV       bool
	replicaCount      int
	statusKVPublisher *statusKVPublisher
}

// New returns a cookie flipper
func New(
	facilityCode string,
	dryrun,
	useStatusKV,
	faultInjection bool,
	concurrency,
	replicaCount int,
	stream events.Stream,
	repository store.Repository,
	logger *logrus.Logger,
) *Flipper {
	id, _ := os.Hostname()

	return &Flipper{
		name:           id,
		facilityCode:   facilityCode,
		dryrun:         dryrun,
		useStatusKV:    useStatusKV,
		faultInjection: faultInjection,
		concurrency:    concurrency,
		replicaCount:   replicaCount,
		syncWG:         &sync.WaitGroup{},
		stream:         stream,
		store:          repository,
		logger:         logger,
	}
}

// Run runs the firmware install worker which listens for events to action.
func (f *Flipper) Run(ctx context.Context) {
	tickerFetchEvents := time.NewTicker(fetchEventsInterval).C

	if err := f.stream.Open(); err != nil {
		f.logger.WithError(err).Error("event stream connection error")
		return
	}

	// returned channel ignored, since this is a Pull based subscription.
	_, err := f.stream.Subscribe(ctx)
	if err != nil {
		f.logger.WithError(err).Error("event stream subscription error")
		return
	}

	f.logger.Info("connected to event stream.")

	f.startFlipperLivenessCheckin(ctx)

	f.statusKVPublisher = newStatusKVPublisher(f.stream, f.replicaCount, f.logger)

	f.logger.WithFields(
		logrus.Fields{
			"replica-count":   f.replicaCount,
			"concurrency":     f.concurrency,
			"dry-run":         f.dryrun,
			"fault-injection": f.faultInjection,
		},
	).Info("cookieflipper running")

Loop:
	for {
		select {
		case <-tickerFetchEvents:
			if f.concurrencyLimit() {
				continue
			}

			f.processEvents(ctx)

		case <-ctx.Done():
			if f.dispatched > 0 {
				continue
			}

			break Loop
		}
	}
}
