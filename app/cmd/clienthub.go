package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

var configFiles []string
var clientHubCmd = &cobra.Command{
	Use:   "client-hub",
	Short: "Client Hub mode",
	Run:   runClientHub,
}

func init() {
	initClientHubFlags()
	rootCmd.AddCommand(clientHubCmd)
}

func initClientHubFlags() {
	clientHubCmd.Flags().StringArrayVar(&configFiles, "configs", []string{"config.yaml"}, "client configs")
}

func runClientHub(cmd *cobra.Command, args []string) {
	var clientConfigs []clientConfig
	for _, filename := range configFiles {
		v := viper.New()
		cfgPath, err := filepath.Abs(filename)
		if err != nil {
			logger.Fatal("failed to parse config file", zap.Error(err))
		}
		v.SetConfigFile(cfgPath)
		if err := v.ReadInConfig(); err != nil {
			logger.Fatal("failed to read client config", zap.Error(err))
		}

		var config clientConfig
		if err := v.Unmarshal(&config); err != nil {
			logger.Fatal("failed to parse client config", zap.Error(err))
		}
		clientConfigs = append(clientConfigs, config)
	}

	var hub runnerHub
	var runResult clientModeRunnerResult
	errChan := make(chan modeError)
	for _, cfg := range clientConfigs {
		runner := initClientRunner(cfg)
		hub.runners = append(hub.runners, runner)
		runResult = runner.Run(errChan)
		if !runResult.OK {
			break
		}
	}
	if !runResult.OK {
		hub.Stop()
		logger.Fatal(runResult.Msg)
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChan)

	select {
	case <-signalChan:
		logger.Info("received signal, shutting down gracefully")
	case modeErr := <-errChan:
		hub.Stop()
		if modeErr.Err != nil {
			logger.Fatal(fmt.Sprintf("mode %s stopped", modeErr.Name), zap.Error(modeErr.Err))
		} else {
			logger.Fatal(fmt.Sprintf("mode %s stopped", modeErr.Name))
		}
	}
}

type runnerHub struct {
	runners []*clientModeRunner
}

func (h *runnerHub) Stop() {
	for _, runner := range h.runners {
		runner.Stop()
	}
}
