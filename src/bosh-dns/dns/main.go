package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"code.cloudfoundry.org/clock"

	dnsconfig "bosh-dns/dns/config"
	handlersconfig "bosh-dns/dns/config/handlers"
	"bosh-dns/dns/server"
	"bosh-dns/dns/server/aliases"
	"bosh-dns/dns/server/handlers"
	"bosh-dns/dns/server/healthiness"
	"bosh-dns/dns/server/records"
	"bosh-dns/dns/server/records/dnsresolver"
	"bosh-dns/dns/shuffle"
	"bosh-dns/healthcheck/healthclient"

	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	"github.com/cloudfoundry/bosh-utils/system"
	boshsys "github.com/cloudfoundry/bosh-utils/system"
	"github.com/miekg/dns"
)

func parseFlags() (string, error) {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to config file")
	flag.Parse()

	if configPath == "" {
		return "", errors.New("--config is a required flag")
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", bosherr.WrapError(err, fmt.Sprintf("Unable to find config file at '%s'", configPath))
	}

	return configPath, nil
}

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {
	logger := boshlog.NewAsyncWriterLogger(boshlog.LevelDebug, os.Stdout)
	logTag := "main"
	defer logger.FlushTimeout(5 * time.Second)

	configPath, err := parseFlags()
	if err != nil {
		logger.Error(logTag, err.Error())
		return 1
	}

	config, err := dnsconfig.LoadFromFile(configPath)
	if err != nil {
		logger.Error(logTag, err.Error())
		return 1
	}

	fs := boshsys.NewOsFileSystem(logger)

	aliasConfiguration, err := aliases.ConfigFromGlob(
		fs,
		aliases.NewFSLoader(fs),
		config.AliasFilesGlob,
	)
	if err != nil {
		logger.Error(logTag, fmt.Sprintf("loading alias configuration: %s", err.Error()))
		return 1
	}

	handlersConfiguration, err := handlersconfig.ConfigFromGlob(
		fs,
		handlersconfig.NewFSLoader(fs),
		config.HandlersFilesGlob,
	)

	if err != nil {
		logger.Error(logTag, fmt.Sprintf("loading handlers configuration: %s", err.Error()))
		return 1
	}

	mux := dns.NewServeMux()
	clock := clock.NewClock()
	repoUpdate := make(chan struct{})

	dnsManager := newDNSManager(logger, clock, fs)
	recursorReader := dnsconfig.NewRecursorReader(dnsManager, config.Address)
	stringShuffler := shuffle.NewStringShuffler()
	err = dnsconfig.ConfigureRecursors(recursorReader, stringShuffler, &config)
	if err != nil {
		logger.Error(logTag, fmt.Sprintf("Unable to configure recursor addresses from os: %s", err.Error()))
		return 1
	}

	var healthWatcher healthiness.HealthWatcher = healthiness.NewNopHealthWatcher()
	if config.Health.Enabled {
		httpClient, err := healthclient.NewHealthClientFromFiles(config.Health.CAFile, config.Health.CertificateFile, config.Health.PrivateKeyFile, logger)
		if err != nil {
			logger.Error(logTag, fmt.Sprintf("Unable to configure health checker %s", err.Error()))
			return 1
		}
		healthChecker := healthiness.NewHealthChecker(httpClient, config.Health.Port)
		checkInterval := time.Duration(config.Health.CheckInterval)
		healthWatcher = healthiness.NewHealthWatcher(healthChecker, clock, checkInterval)
	}

	shutdown := make(chan struct{})

	fileReader := records.NewFileReader(config.RecordsFile, system.NewOsFileSystem(logger), clock, logger, repoUpdate)
	recordSet, err := records.NewRecordSet(fileReader, aliasConfiguration, healthWatcher, uint(config.Health.MaxTrackedQueries), shutdown, logger)

	localDomain := dnsresolver.NewLocalDomain(logger, recordSet, shuffle.New())
	discoveryHandler := handlers.NewDiscoveryHandler(logger, localDomain)

	handlerRegistrar := handlers.NewHandlerRegistrar(logger, clock, recordSet, mux, discoveryHandler)

	mux.Handle("arpa.", handlers.NewRequestLoggerHandler(handlers.NewArpaHandler(logger), clock, logger))

	exchangerFactory := handlers.NewExchangerFactory(time.Duration(config.RecursorTimeout))
	handlerFactory := handlers.NewFactory(exchangerFactory, clock, stringShuffler, logger)

	delegatingHandlers, err := handlersConfiguration.GenerateHandlers(handlerFactory)
	if err != nil {
		logger.Error(logTag, err.Error())
		return 1
	}
	for domain, handler := range delegatingHandlers {
		mux.Handle(domain, handlers.NewRequestLoggerHandler(handler, clock, logger))
	}

	upchecks := []server.Upcheck{}
	for _, upcheckDomain := range config.UpcheckDomains {
		mux.Handle(upcheckDomain, handlers.NewRequestLoggerHandler(handlers.NewUpcheckHandler(logger), clock, logger))
		upchecks = append(upchecks, server.NewDNSAnswerValidatingUpcheck(fmt.Sprintf("%s:%d", config.Address, config.Port), upcheckDomain, "udp"))
		upchecks = append(upchecks, server.NewDNSAnswerValidatingUpcheck(fmt.Sprintf("%s:%d", config.Address, config.Port), upcheckDomain, "tcp"))
	}

	recursorPool := handlers.NewFailoverRecursorPool(config.Recursors, logger)
	forwardHandler := handlers.NewForwardHandler(recursorPool, exchangerFactory, clock, logger)
	if config.Cache.Enabled {
		mux.Handle(".", handlers.NewCachingDNSHandler(forwardHandler))
	} else {
		mux.Handle(".", forwardHandler)
	}

	bindAddress := fmt.Sprintf("%s:%d", config.Address, config.Port)
	dnsServer := server.New(
		[]server.DNSServer{
			&dns.Server{Addr: bindAddress, Net: "tcp", Handler: mux},
			&dns.Server{Addr: bindAddress, Net: "udp", Handler: mux, UDPSize: 65535},
		},
		upchecks,
		time.Duration(config.Timeout),
		time.Duration(5*time.Second),
		shutdown,
		logger,
	)

	go func() {
		err := handlerRegistrar.Run(shutdown)
		if err != nil {
			logger.Error(logTag, fmt.Sprintf("could not start handler registrar: %s", err.Error()))
		}
	}()

	go healthWatcher.Run(shutdown)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)

	go func() {
		<-sigterm
		close(repoUpdate)
		close(shutdown)
	}()

	if err := dnsServer.Run(); err != nil {
		logger.Error(logTag, err.Error())
		return 1
	}

	return 0
}
