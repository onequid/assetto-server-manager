package main

import (
	"context"
	"flag"
	"os"
	"os/signal"

	"github.com/sirupsen/logrus"

	"justapengu.in/acsm/internal/acserver"
	"justapengu.in/acsm/internal/acserver/plugins"
)

var configPath string

func init() {
	flag.StringVar(&configPath, "c", "./config.yml", "config path")
	flag.Parse()
}

func main() {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	logger.SetLevel(logrus.DebugLevel)

	logger.Infof("Starting acServer version 2 by Emperor Servers - https://emperorservers.com/")

	config, err := readConfig()

	if err != nil {
		logger.WithError(err).Fatal("Could not read config at ./config.yml")
	}

	var plugin acserver.Plugin

	if config.ServerConfig.UDPPluginLocalPort > 0 && config.ServerConfig.UDPPluginAddress != "" {
		plugin, err = plugins.NewUDPPlugin(config.ServerConfig.UDPPluginLocalPort, config.ServerConfig.UDPPluginAddress)

		if err != nil {
			logger.WithError(err).Fatal("Could not initialise UDP plugin")
		}
	}

	server, err := acserver.NewServer(context.Background(), ".", config.ServerConfig, config.RaceConfig, config.EntryList, config.CustomChecksums, logger, plugin, 0)

	if err != nil {
		logger.WithError(err).Fatal("Could not initialise server")
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		for range c {
			if err := server.Stop(true); err != nil {
				logger.WithError(err).Fatal("Could not stop server")
			}

			os.Exit(0)
		}
	}()

	err = server.Run()

	if err != nil {
		logger.WithError(err).Fatal("could not run server")
	}

	logger.Infof("Server stopped. Exiting")
}

// TempConfig is temporary until we do server manager integration
type TempConfig struct {
	ServerConfig    *acserver.ServerConfig        `json:"server_config" yaml:"server_config"`
	RaceConfig      *acserver.EventConfig         `json:"race_config" yaml:"race_config"`
	EntryList       acserver.EntryList            `json:"entry_list" yaml:"entry_list"`
	CustomChecksums []acserver.CustomChecksumFile `json:"checksums" yaml:"checksums"`
}

func readConfig() (*TempConfig, error) {
	return readLegacyConfigs()
	/*
		var conf *TempConfig

		f, err := os.Open(configPath)

		if err != nil {
			return nil, err
		}

		defer f.Close()

		if err := yaml.NewDecoder(f).Decode(&conf); err != nil {
			return nil, err
		}

		return conf, nil
	*/
}