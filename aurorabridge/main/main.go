// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"

	"github.com/uber/peloton/.gen/peloton/api/v0/respool"
	statelesssvc "github.com/uber/peloton/.gen/peloton/api/v1alpha/job/stateless/svc"
	podsvc "github.com/uber/peloton/.gen/peloton/api/v1alpha/pod/svc"
	"github.com/uber/peloton/.gen/thrift/aurora/api/auroraschedulermanagerserver"
	"github.com/uber/peloton/.gen/thrift/aurora/api/readonlyschedulerserver"

	"github.com/uber/peloton/aurorabridge"
	"github.com/uber/peloton/common"
	"github.com/uber/peloton/common/buildversion"
	"github.com/uber/peloton/common/config"
	"github.com/uber/peloton/common/health"
	"github.com/uber/peloton/common/logging"
	"github.com/uber/peloton/common/metrics"
	"github.com/uber/peloton/common/rpc"
	"github.com/uber/peloton/leader"
	"github.com/uber/peloton/yarpc/peer"

	log "github.com/sirupsen/logrus"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/api/transport"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	version string
	app     = kingpin.New("peloton-aurorabridge", "Peloton Aurora bridge")

	debug = app.Flag(
		"debug", "enable debug mode (print full json responses)").
		Short('d').
		Default("false").
		Envar("ENABLE_DEBUG_LOGGING").
		Bool()

	enableSentry = app.Flag(
		"enable-sentry", "enable logging hook up to sentry").
		Default("false").
		Envar("ENABLE_SENTRY_LOGGING").
		Bool()

	cfgFiles = app.Flag(
		"config",
		"YAML config files (can be provided multiple times to merge configs)").
		Short('c').
		Required().
		ExistingFiles()

	electionZkServers = app.Flag(
		"election-zk-server",
		"Election Zookeeper servers. Specify multiple times for multiple servers "+
			"(election.zk_servers override) (set $ELECTION_ZK_SERVERS to override)").
		Envar("ELECTION_ZK_SERVERS").
		Strings()

	datacenter = app.Flag(
		"datacenter", "Datacenter name").
		Default("").
		Envar("DATACENTER").
		String()

	httpPort = app.Flag(
		"http-port", "Aurora Bridge HTTP port (aurorabridge.http_port override) "+
			"(set $PORT to override)").
		Default("8082").
		Envar("HTTP_PORT").
		Int()

	grpcPort = app.Flag(
		"grpc-port", "Aurora Bridge gRPC port (aurorabridge.grpc_port override) "+
			"(set $PORT to override)").
		Envar("GRPC_PORT").
		Int()
)

func main() {
	app.Version(version)
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))

	log.SetFormatter(&log.JSONFormatter{})

	initialLevel := log.InfoLevel
	if *debug {
		initialLevel = log.DebugLevel
	}
	log.SetLevel(initialLevel)

	var cfg Config
	if err := config.Parse(&cfg, *cfgFiles...); err != nil {
		log.Fatalf("Error parsing yaml config: %s", err)
	}

	if *enableSentry {
		logging.ConfigureSentry(&cfg.SentryConfig)
	}

	if len(*electionZkServers) > 0 {
		cfg.Election.ZKServers = *electionZkServers
	}

	if *httpPort != 0 {
		cfg.HTTPPort = *httpPort
	}

	if *grpcPort != 0 {
		cfg.GRPCPort = *grpcPort
	}

	log.WithField("config", cfg).Info("Loaded AuroraBridge configuration")

	rootScope, scopeCloser, mux := metrics.InitMetricScope(
		&cfg.Metrics,
		common.PelotonAuroraBridge,
		metrics.TallyFlushInterval,
	)
	defer scopeCloser.Close()

	mux.HandleFunc(
		logging.LevelOverwrite,
		logging.LevelOverwriteHandler(initialLevel))

	mux.HandleFunc(buildversion.Get, buildversion.Handler(version))

	// Create both HTTP and GRPC inbounds
	inbounds := rpc.NewAuroraBridgeInbounds(
		cfg.HTTPPort,
		cfg.GRPCPort, // dummy grpc port for aurora bridge
		mux)

	// all leader discovery metrics share a scope (and will be tagged
	// with role={role})
	discoveryScope := rootScope.SubScope("discovery")

	// setup the discovery service to detect jobmgr leaders and
	// configure the YARPC Peer dynamically
	jobmgrTransport := rpc.NewTransport()
	jobmgrPeerChooser, err := peer.NewSmartChooser(
		cfg.Election,
		discoveryScope,
		common.JobManagerRole,
		jobmgrTransport,
	)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"role":  common.JobManagerRole,
		}).Fatal("Could not create smart peer chooser")
	}
	defer jobmgrPeerChooser.Stop()

	resmgrTransport := rpc.NewTransport()
	resmgrPeerChooser, err := peer.NewSmartChooser(
		cfg.Election,
		discoveryScope,
		common.ResourceManagerRole,
		resmgrTransport,
	)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"role":  common.ResourceManagerRole,
		}).Fatal("Could not create smart peer chooser")
	}
	defer resmgrPeerChooser.Stop()

	outbounds := yarpc.Outbounds{
		common.PelotonJobManager: transport.Outbounds{
			Unary: jobmgrTransport.NewOutbound(jobmgrPeerChooser),
		},
		common.PelotonResourceManager: transport.Outbounds{
			Unary: resmgrTransport.NewOutbound(resmgrPeerChooser),
		},
	}

	dispatcher := yarpc.NewDispatcher(yarpc.Config{
		Name:      common.PelotonAuroraBridge,
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Metrics: yarpc.MetricsConfig{
			Tally: rootScope,
		},
	})

	jobClient := statelesssvc.NewJobServiceYARPCClient(
		dispatcher.ClientConfig(common.PelotonJobManager))

	podClient := podsvc.NewPodServiceYARPCClient(
		dispatcher.ClientConfig(common.PelotonJobManager))

	respoolClient := respool.NewResourceManagerYARPCClient(
		dispatcher.ClientConfig(common.PelotonResourceManager))

	// Start the dispatcher before we register the aurorabridge handler, since we'll
	// need to make some outbound requests to get things setup.
	if err := dispatcher.Start(); err != nil {
		log.Fatalf("Could not start rpc server: %v", err)
	}

	server := aurorabridge.NewServer(cfg.HTTPPort)

	candidate, err := leader.NewCandidate(
		cfg.Election,
		rootScope,
		common.PelotonAuroraBridgeRole,
		server,
	)
	if err != nil {
		log.Fatalf("Unable to create leader candidate: %v", err)
	}

	respoolLoader := aurorabridge.NewRespoolLoader(cfg.RespoolLoader, respoolClient)

	handler, err := aurorabridge.NewServiceHandler(
		cfg.ServiceHandler,
		rootScope,
		jobClient,
		podClient,
		respoolLoader,
	)
	if err != nil {
		log.Fatalf("Unable to create service handler: %v", err)
	}

	dispatcher.Register(auroraschedulermanagerserver.New(handler))
	dispatcher.Register(readonlyschedulerserver.New(handler))

	if err := candidate.Start(); err != nil {
		log.Fatalf("Unable to start leader candidate: %v", err)
	}
	defer candidate.Stop()

	log.WithFields(log.Fields{
		"httpPort": cfg.HTTPPort,
	}).Info("Started Aurora Bridge")

	// we can *honestly* say the server is booted up now
	health.InitHeartbeat(rootScope, cfg.Health, candidate)

	select {}
}
