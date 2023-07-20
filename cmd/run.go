package cmd

import (
	"context"
	"log"

	"github.com/equinix-labs/otel-init-go/otelinit"
	"github.com/metal-toolbox/cookieflipper/app"
	"github.com/metal-toolbox/cookieflipper/internal/flipper"
	"github.com/metal-toolbox/cookieflipper/internal/metrics"
	"github.com/metal-toolbox/cookieflipper/internal/store"
	"github.com/metal-toolbox/cookieflipper/internal/version"
	"github.com/metal-toolbox/cookieflipper/pkg/types"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"go.hollow.sh/toolbox/events"

	// nolint:gosec // profiling endpoint listens on localhost.
	_ "net/http/pprof"
)

var cmdRun = &cobra.Command{
	Use:   "run",
	Short: "Run flasher service to listen for events and install firmware",
	Run: func(cmd *cobra.Command, args []string) {
		runWorker(cmd.Context())
	},
}

// run worker command
var (
	useStatusKV    bool
	dryrun         bool
	faultInjection bool
	facilityCode   string
	storeKind      string
	replicas       int
)

var (
	ErrInventoryStore = errors.New("inventory store error")
)

func runWorker(ctx context.Context) {
	theApp, termCh, err := app.New(
		types.AppKindFlipper,
		types.StoreKind(storeKind),
		cfgFile,
		logLevel,
		enableProfiling,
	)
	if err != nil {
		log.Fatal(err)
	}

	// serve metrics endpoint
	metrics.ListenAndServe()
	version.ExportBuildInfoMetric()

	ctx, otelShutdown := otelinit.InitOpenTelemetry(ctx, "flasher")
	defer otelShutdown(ctx)

	// Setup cancel context with cancel func.
	ctx, cancelFunc := context.WithCancel(ctx)

	// routine listens for termination signal and cancels the context
	go func() {
		<-termCh
		theApp.Logger.Info("got TERM signal, exiting...")
		cancelFunc()
	}()

	inv := store.New(theApp.Config.ServerserviceOptions)

	stream, err := events.NewStream(*theApp.Config.NatsOptions)
	if err != nil {
		theApp.Logger.Fatal(err)
	}

	if useStatusKV && facilityCode == "" {
		theApp.Logger.Fatal("--use-kv flag requires a --facility-code parameter")
	}

	w := flipper.New(
		facilityCode,
		dryrun,
		useStatusKV,
		faultInjection,
		theApp.Config.Concurrency,
		replicas,
		stream,
		inv,
		theApp.Logger,
	)

	w.Run(ctx)
}

func init() {
	cmdRun.PersistentFlags().StringVar(&storeKind, "store", "", "Inventory store to lookup devices for update - 'serverservice' or an inventory file with a .yml/.yaml extenstion")
	cmdRun.PersistentFlags().BoolVarP(&dryrun, "dry-run", "", false, "In dryrun mode, the worker actions the task without installing firmware")
	cmdRun.PersistentFlags().BoolVarP(&useStatusKV, "use-kv", "", false, "When this is true, flasher writes status to a NATS KV store instead of sending reply messages (requires --facility-code)")
	cmdRun.PersistentFlags().BoolVarP(&faultInjection, "fault-injection", "", false, "Tasks can include a Fault attribute to allow fault injection for development purposes")
	cmdRun.PersistentFlags().IntVarP(&replicas, "replica-count", "r", 3, "The number of replicas to use for NATS data")
	cmdRun.PersistentFlags().StringVar(&facilityCode, "facility-code", "", "The facility code this flasher instance is associated with")

	if err := cmdRun.MarkPersistentFlagRequired("store"); err != nil {
		log.Fatal(err)
	}

	rootCmd.AddCommand(cmdRun)
}
