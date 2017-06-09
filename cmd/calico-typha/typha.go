// Copyright (c) 2016-2017 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docopt/docopt-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/projectcalico/libcalico-go/lib/backend"
	bapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/typha/pkg/buildinfo"
	"github.com/projectcalico/typha/pkg/calc"
	"github.com/projectcalico/typha/pkg/config"
	"github.com/projectcalico/typha/pkg/logutils"
	"github.com/projectcalico/typha/pkg/snapcache"
	"github.com/projectcalico/typha/pkg/syncserver"
)

const usage = `Typha, Calico's fan-out proxy.

Usage:
  calico-typha [options]

Options:
  -c --config-file=<filename>  Config file to load [default: /etc/calico/typha.cfg].
  --version                    Print the version and exit.
`

// main is the entry point to the calico-typha binary.
//
// Its main role is to sequence Typha's startup by:
//
// Initialising early logging config (log format and early debug settings).
//
// Parsing command line parameters.
//
// Loading datastore configuration from the environment or config file.
//
// Loading more configuration from the datastore (this is retried until success).
//
// Starting the fan-out proxy.
//
// Starting the usage reporting and prometheus metrics endpoint threads (if configured).
//
// Then, it defers to monitorAndManageShutdown(), which blocks until one of the components
// fails, then attempts a graceful shutdown.  At that point, all the processing is in
// background goroutines.
//
// To avoid having to maintain rarely-used code paths, Typha handles updates to its
// main config parameters by exiting and allowing itself to be restarted by the init
// daemon.
func main() {
	// Go's RNG is not seeded by default.  Do that now.
	rand.Seed(time.Now().UTC().UnixNano())

	// Special-case handling for environment variable-configured logging:
	// Initialise early so we can trace out config parsing.
	logutils.ConfigureEarlyLogging()

	// Parse command-line args.
	version := "Version:            " + buildinfo.GitVersion + "\n" +
		"Full git commit ID: " + buildinfo.GitRevision + "\n" +
		"Build date:         " + buildinfo.BuildDate + "\n"
	arguments, err := docopt.Parse(usage, nil, true, version, false)
	if err != nil {
		println(usage)
		log.Fatalf("Failed to parse usage, exiting: %v", err)
	}
	buildInfoLogCxt := log.WithFields(log.Fields{
		"version":    buildinfo.GitVersion,
		"buildDate":  buildinfo.BuildDate,
		"gitCommit":  buildinfo.GitRevision,
		"GOMAXPROCS": runtime.GOMAXPROCS(0),
	})
	buildInfoLogCxt.Info("Typha starting up")
	log.Infof("Command line arguments: %v", arguments)

	// Load the configuration from all the different sources including the
	// datastore and merge. Keep retrying on failure.  We'll sit in this
	// loop until the datastore is ready.
	log.Infof("Loading configuration...")
	var datastore bapi.Client
	var configParams *config.Config
configRetry:
	for {
		// Load locally-defined config, including the datastore connection
		// parameters. First the environment variables.
		configParams = config.New()
		envConfig := config.LoadConfigFromEnvironment(os.Environ())
		// Then, the config file.
		configFile := arguments["--config-file"].(string)
		fileConfig, err := config.LoadConfigFile(configFile)
		if err != nil {
			log.WithError(err).WithField("configFile", configFile).Error(
				"Failed to load configuration file")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		// Parse and merge the local config.
		configParams.UpdateFrom(envConfig, config.EnvironmentVariable)
		if configParams.Err != nil {
			log.WithError(configParams.Err).WithField("configFile", configFile).Error(
				"Failed to parse configuration environment variable")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		configParams.UpdateFrom(fileConfig, config.ConfigFile)
		if configParams.Err != nil {
			log.WithError(configParams.Err).WithField("configFile", configFile).Error(
				"Failed to parse configuration file")
			time.Sleep(1 * time.Second)
			continue configRetry
		}

		// We should now have enough config to connect to the datastore
		// so we can load the remainder of the config.
		datastoreConfig := configParams.DatastoreConfig()
		datastore, err = backend.NewClient(datastoreConfig)
		if err != nil {
			log.WithError(err).Error("Failed to connect to datastore")
			time.Sleep(1 * time.Second)
			continue configRetry
		}

		configParams.Validate()
		if configParams.Err != nil {
			log.WithError(configParams.Err).Error(
				"Failed to parse/validate configuration from datastore.")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		break configRetry
	}

	// If we get here, we've loaded the configuration successfully.
	// Update log levels before we do anything else.
	logutils.ConfigureLogging(configParams)
	// Since we may have enabled more logging, log with the build context
	// again.
	buildInfoLogCxt.WithField("config", configParams).Info(
		"Successfully loaded configuration.")

	// Make sure the datastore is initialized, otherwise the Syncer may spin, looking for
	// non-existent resources.
	datastore.EnsureInitialized()

	// Now create the Syncer; our caching layer and the TCP server.

	// Get a Syncer from the datastore, which will feed the validator layer with updates.
	syncerToValidator := calc.NewSyncerCallbacksDecoupler()
	syncer := datastore.Syncer(syncerToValidator)
	log.Debugf("Created Syncer: %#v", syncer)

	// Create the validator, which sits between the syncer and the cache.
	validatorToCache := calc.NewSyncerCallbacksDecoupler()
	validator := calc.NewValidationFilter(validatorToCache)

	// Create our snapshot cache, which stores point-in-time copies of the datastore contents.
	cache := snapcache.New(snapcache.Config{
		MaxBatchSize: configParams.SnapshotCacheMaxBatchSize,
	})

	// Create the server, which listens for connections from Felix.
	server := syncserver.New(
		cache,
		syncserver.Config{
			MaxMessageSize:          configParams.ServerMaxMessageSize,
			MinBatchingAgeThreshold: configParams.ServerMinBatchingAgeThresholdSecs,
			MaxFallBehind:           configParams.ServerMaxFallBehindSecs,
			PingInterval:            configParams.ServerPingIntervalSecs,
			PongTimeout:             configParams.ServerPongTimeoutSecs,
		},
	)

	// Now we've connected everything up, start the background processing threads.
	log.Info("Starting the datastore Syncer/cache layer")
	syncer.Start()
	go syncerToValidator.SendTo(validator)
	go validatorToCache.SendTo(cache)
	cache.Start(context.Background())
	go server.Serve(context.Background())
	log.Info("Started the datastore Syncer/cache layer/server.")

	if configParams.PrometheusMetricsEnabled {
		log.Info("Prometheus metrics enabled.  Starting server.")
		go servePrometheusMetrics(configParams)
	}

	// Hook and process the signals we care about
	usr1SignalChan := make(chan os.Signal, 1)
	signal.Notify(usr1SignalChan, syscall.SIGUSR1)
	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, syscall.SIGTERM)
	for {
		select {
		case <-termChan:
			log.Fatal("Received SIGTERM, shutting down")
		case <-usr1SignalChan:
			log.Info("Received SIGUSR1, emitting heap profile")
			dumpHeapMemoryProfile(configParams)
		}
	}
}

// TODO Typha: Share with Felix.
func dumpHeapMemoryProfile(configParams *config.Config) {
	// If a memory profile file name is configured, dump a heap memory profile.  If the
	// configured filename includes "<timestamp>", that will be replaced with a stamp indicating
	// the current time.
	memProfFileName := configParams.DebugMemoryProfilePath
	if memProfFileName != "" {
		logCxt := log.WithField("file", memProfFileName)
		logCxt.Info("Asked to create a memory profile.")

		// If the configured file name includes "<timestamp>", replace that with the current
		// time.
		if strings.Contains(memProfFileName, "<timestamp>") {
			timestamp := time.Now().Format("2006-01-02-15:04:05")
			memProfFileName = strings.Replace(memProfFileName, "<timestamp>", timestamp, 1)
			logCxt = log.WithField("file", memProfFileName)
		}

		// Open a file with that name.
		memProfFile, err := os.Create(memProfFileName)
		if err != nil {
			logCxt.WithError(err).Fatal("Could not create memory profile file")
			memProfFile = nil
		} else {
			defer memProfFile.Close()
			logCxt.Info("Writing memory profile...")
			// The initial resync uses a lot of scratch space so now is
			// a good time to force a GC and return any RAM that we can.
			debug.FreeOSMemory()
			if err := pprof.WriteHeapProfile(memProfFile); err != nil {
				logCxt.WithError(err).Fatal("Could not write memory profile")
			}
			logCxt.Info("Finished writing memory profile")
		}
	}
}

// TODO Typha: Share with Felix.
func servePrometheusMetrics(configParams *config.Config) {
	for {
		log.WithField("port", configParams.PrometheusMetricsPort).Info("Starting prometheus metrics endpoint")
		if configParams.PrometheusGoMetricsEnabled && configParams.PrometheusProcessMetricsEnabled {
			log.Info("Including Golang & Process metrics")
		} else {
			if !configParams.PrometheusGoMetricsEnabled {
				log.Info("Discarding Golang metrics")
				prometheus.Unregister(prometheus.NewGoCollector())
			}
			if !configParams.PrometheusProcessMetricsEnabled {
				log.Info("Discarding process metrics")
				prometheus.Unregister(prometheus.NewProcessCollector(os.Getpid(), ""))
			}
		}
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(fmt.Sprintf(":%v", configParams.PrometheusMetricsPort), nil)
		log.WithError(err).Error(
			"Prometheus metrics endpoint failed, trying to restart it...")
		time.Sleep(1 * time.Second)
	}
}
