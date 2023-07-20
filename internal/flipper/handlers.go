package flipper

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	cptypes "github.com/metal-toolbox/conditionorc/pkg/types"
	"github.com/metal-toolbox/cookieflipper/internal/metrics"
	"github.com/metal-toolbox/cookieflipper/pkg/types"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.hollow.sh/toolbox/events"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type taskState int

const (
	// conditionInprogressTicker is the interval at which condtion in progress
	// will ack themselves as in progress on the event stream.
	//
	// This value should be set to less than the event stream Ack timeout value.
	conditionInprogressTick = 3 * time.Minute

	notStarted    taskState = iota
	inProgress              // another flipper has started it, is still around and updated recently
	complete                // task is done
	orphaned                // the flipper that started this task doesn't exist anymore
	indeterminate           // we got an error in the process of making the check
)

func (f *Flipper) processEvents(ctx context.Context) {
	// XXX: consider having a separate context for message retrieval
	msgs, err := f.stream.PullMsg(ctx, 1)
	switch {
	case err == nil:
	case errors.Is(err, nats.ErrTimeout):
		f.logger.WithFields(
			logrus.Fields{"err": err.Error()},
		).Trace("no new events")
	default:
		f.logger.WithFields(
			logrus.Fields{"err": err.Error()},
		).Warn("retrieving new messages")
		metrics.NATSError("pull-msg")
	}

	for _, msg := range msgs {
		if ctx.Err() != nil || f.concurrencyLimit() {
			f.eventNak(msg)

			return
		}

		// spawn msg process handler
		f.syncWG.Add(1)

		go func(msg events.Message) {
			defer f.syncWG.Done()

			atomic.AddInt32(&f.dispatched, 1)
			defer atomic.AddInt32(&f.dispatched, -1)

			f.processSingleEvent(ctx, msg)
		}(msg)
	}
}

func (f *Flipper) concurrencyLimit() bool {
	return int(f.dispatched) >= f.concurrency
}

func (f *Flipper) eventAckInProgress(event events.Message) {
	if err := event.InProgress(); err != nil {
		metrics.NATSError("ack-in-progress")
		f.logger.WithError(err).Warn("event Ack Inprogress error")
	}
}

func (f *Flipper) eventAckComplete(event events.Message) {
	if err := event.Ack(); err != nil {
		f.logger.WithError(err).Warn("event Ack error")
	}
}

func (f *Flipper) eventNak(event events.Message) {
	if err := event.Nak(); err != nil {
		metrics.NATSError("nak")
		f.logger.WithError(err).Warn("event Nak error")
	}
}

func (f *Flipper) registerEventCounter(valid bool, response string) {
	metrics.EventsCounter.With(
		prometheus.Labels{
			"valid":    strconv.FormatBool(valid),
			"response": response,
		}).Inc()
}

func (f *Flipper) processSingleEvent(ctx context.Context, e events.Message) {
	// extract parent trace context from the event if any.
	ctx = e.ExtractOtelTraceContext(ctx)

	ctx, span := otel.Tracer(pkgName).Start(
		ctx,
		"flipper.processSingleEvent",
	)
	defer span.End()

	// parse condition from event
	condition, err := conditionFromEvent(e)
	if err != nil {
		f.logger.WithError(err).WithField(
			"subject", e.Subject()).Warn("unable to retrieve condition from message")

		f.registerEventCounter(false, "ack")
		f.eventAckComplete(e)

		return
	}

	// parse parameters from condition
	params, err := f.parametersFromCondition(condition)
	if err != nil {
		f.logger.WithError(err).WithField(
			"subject", e.Subject()).Warn("unable to retrieve parameters from condition")

		f.registerEventCounter(false, "ack")
		f.eventAckComplete(e)

		return
	}

	// fetch cookie from store
	cookie, err := f.store.CookieByName(ctx, params.CookieID.String())
	if err != nil {
		f.logger.WithFields(logrus.Fields{
			"cookieID":    params.CookieID.String(),
			"conditionID": condition.ID,
			"err":         err.Error(),
		}).Warn("cookie lookup error")

		f.registerEventCounter(true, "nack")
		f.eventNak(e) // have the message bus re-deliver the message
		metrics.RegisterSpanEvent(
			span,
			condition,
			f.id.String(),
			params.CookieID.String(),
			"sent nack, store query error",
		)

		return
	}

	flipCtx, cancel := context.WithTimeout(ctx, conditionTimeout)
	defer cancel()

	defer f.registerEventCounter(true, "ack")
	defer f.eventAckComplete(e)
	metrics.RegisterSpanEvent(
		span,
		condition,
		f.id.String(),
		params.CookieID.String(),
		"sent ack, condition fulfilled",
	)

	f.flipCookieWithMonitor(flipCtx, cookie, condition.ID, params, e)
}

func conditionFromEvent(e events.Message) (*cptypes.Condition, error) {
	data := e.Data()
	if data == nil {
		return nil, errors.New("data field empty")
	}

	condition := &cptypes.Condition{}
	if err := json.Unmarshal(data, condition); err != nil {
		return nil, errors.Wrap(errConditionDeserialize, err.Error())
	}

	return condition, nil
}

func (f *Flipper) parametersFromCondition(condition *cptypes.Condition) (*types.Parameters, error) {
	errParameters := errors.New("condition parameters error")

	parameters := &types.Parameters{}
	if err := json.Unmarshal(condition.Parameters, parameters); err != nil {
		return nil, errors.Wrap(errParameters, err.Error())
	}

	if f.faultInjection && condition.Fault != nil {
		parameters.Fault = condition.Fault
	}

	return parameters, nil
}

func (f *Flipper) flipCookieWithMonitor(ctx context.Context, cookie *types.Cookie, conditionID uuid.UUID, params *types.Parameters, e events.Message) {
	// the runTask method is expected to close this channel to indicate its done
	doneCh := make(chan bool)

	// monitor sends in progress ack's until the cookie flipper method returns.
	monitor := func() {
		defer f.syncWG.Done()

		ticker := time.NewTicker(conditionInprogressTick)
		defer ticker.Stop()

	Loop:
		for {
			select {
			case <-ticker.C:
				f.eventAckInProgress(e)
			case <-doneCh:
				break Loop
			}
		}
	}

	f.syncWG.Add(1)

	go monitor()

	f.flipCookie(ctx, cookie, conditionID, params, doneCh)

	<-doneCh
}

// where the flipping actually happens
func (f *Flipper) flipCookie(ctx context.Context, cookie *types.Cookie, conditionID uuid.UUID, params *types.Parameters, doneCh chan bool) {
	defer close(doneCh)
	startTS := time.Now()

	f.logger.WithFields(logrus.Fields{
		"cookieID":    cookie.ID,
		"conditionID": conditionID,
	}).Info("actioning condition for cookie")

	f.publishStatus(
		ctx,
		params.CookieID.String(),
		cptypes.Active,
		"actioning condition for cookie",
	)

	if params.Fault != nil {
		if params.Fault.FailAt != "" {
			f.publishStatus(
				ctx,
				params.CookieID.String(),
				cptypes.Failed,
				"failed due to induced fault",
			)

			f.registerConditionMetrics(startTS, string(cptypes.Failed))
		}

		if params.Fault.Panic {
			panic("fault induced panic")
		}

		d, err := time.ParseDuration(params.Fault.DelayDuration)
		if err == nil {
			time.Sleep(d)
		}
	}

	for i := 0; i <= params.Flips; i++ {
		f.publishStatus(
			ctx,
			params.CookieID.String(),
			cptypes.Active,
			fmt.Sprintf("%d flipping cookie", i),
		)

		time.Sleep(params.FlipDelay)
	}

	f.registerConditionMetrics(startTS, string(cptypes.Succeeded))

	f.logger.WithFields(logrus.Fields{
		"cookieID":    cookie.ID,
		"conditionID": conditionID,
	}).Info("condition for cookie was fullfilled")

}

func (f *Flipper) publishStatus(ctx context.Context, cookieID string, state cptypes.ConditionState, status string) []byte {
	sv := &types.StatusValue{
		WorkerID: f.id.String(),
		Target:   cookieID,
		TraceID:  trace.SpanFromContext(ctx).SpanContext().TraceID().String(),
		SpanID:   trace.SpanFromContext(ctx).SpanContext().SpanID().String(),
		State:    string(state),
		Status:   statusInfoJSON(status),
		// ResourceVersion:  XXX: the handler context has no concept of this! does this make
		// sense at the controller-level?
		UpdatedAt: time.Now(),
	}

	return sv.MustBytes()
}

func statusInfoJSON(s string) json.RawMessage {
	return []byte(fmt.Sprintf("{%q: %q}", "msg", s))
}

func (f *Flipper) registerConditionMetrics(startTS time.Time, state string) {
	metrics.ConditionRunTimeSummary.With(
		prometheus.Labels{
			"condition": string(cptypes.FirmwareInstall),
			"state":     state,
		},
	).Observe(time.Since(startTS).Seconds())
}
