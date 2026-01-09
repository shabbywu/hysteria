package cmd

import (
	"errors"
	"fmt"
	"github.com/apernet/hysteria/app/v2/internal/http"
	"github.com/apernet/hysteria/app/v2/internal/proxymux"
	"github.com/apernet/hysteria/app/v2/internal/socks5"
	"github.com/apernet/hysteria/core/v2/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"os"
	"os/signal"
	"syscall"
)

type clientHubConfig struct {
	// 并行客户端
	ParallelClientConfigs []clientConfig    `mapstructure:"parallelClients"`
	RoundRobinConfig      *roundRobinConfig `mapstructure:"roundRobin"`
}

type roundRobinConfig struct {
	ClientConfigs []clientConfig `mapstructure:"clients"`
	HTTP          *httpConfig    `mapstructure:"http"`
	SOCKS5        *socks5Config  `mapstructure:"socks5"`
}

var clientHubCmd = &cobra.Command{
	Use:   "client-hub",
	Short: "Client Hub mode",
	Run:   runClientHubCmd,
}

func init() {
	rootCmd.AddCommand(clientHubCmd)
}

func runClientHubCmd(cmd *cobra.Command, args []string) {
	logger.Info("client-hub mode")
	runClientHub(defaultViper)
}

func runClientHub(viper *viper.Viper) {
	var hubConfig clientHubConfig
	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("failed to read client config", zap.Error(err))
	}

	if err := viper.Unmarshal(&hubConfig); err != nil {
		logger.Fatal("failed to parse client config", zap.Error(err))
	}

	errChan := make(chan modeError)
	var parallelHub *parallelRunnerHub
	if len(hubConfig.ParallelClientConfigs) > 0 {
		parallelHub = runParallelRunner(&hubConfig, errChan)
	}
	defer parallelHub.Stop()

	roundRobinRunner := newRoundRobinRunner(&hubConfig)
	roundRobinRunner.Run(errChan)
	defer roundRobinRunner.Stop()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChan)

	select {
	case <-signalChan:
		logger.Info("received signal, shutting down gracefully")
	case modeErr := <-errChan:
		parallelHub.Stop()
		roundRobinRunner.Stop()
		if modeErr.Err != nil {
			logger.Fatal(fmt.Sprintf("mode %s stopped", modeErr.Name), zap.Error(modeErr.Err))
		} else {
			logger.Fatal(fmt.Sprintf("mode %s stopped", modeErr.Name))
		}
	}
}

type parallelRunnerHub struct {
	runners []*clientModeRunner
}

func (h *parallelRunnerHub) Stop() {
	if h == nil {
		return
	}
	for _, runner := range h.runners {
		runner.Stop()
	}
}

func runParallelRunner(hubConfig *clientHubConfig, errChan chan modeError) *parallelRunnerHub {
	var hub parallelRunnerHub
	var runResult clientModeRunnerResult
	for _, cfg := range hubConfig.ParallelClientConfigs {
		runner := initClientRunner(cfg)
		hub.runners = append(hub.runners, runner)
		runResult = runner.Run(errChan)
		if !runResult.OK {
			break
		}
	}
	if !runResult.OK && len(hubConfig.ParallelClientConfigs) > 0 {
		hub.Stop()
		logger.Fatal(runResult.Msg)
	}
	return &hub
}

func newRoundRobinRunner(hubConfig *clientHubConfig) *roundRobinRunner {
	if hubConfig.RoundRobinConfig == nil {
		return nil
	}
	runner := &roundRobinRunner{
		ModeMap:          make(map[string]func() error),
		connections:      make(map[int]client.Client),
		roundRobinConfig: hubConfig.RoundRobinConfig,
	}
	if hubConfig.RoundRobinConfig.SOCKS5 != nil {
		runner.ModeMap["SOCKS5 server"] = runner.listenSocket5
	}
	if hubConfig.RoundRobinConfig.HTTP != nil {
		runner.ModeMap["HTTP server"] = runner.listenHttp
	}
	return runner
}

type roundRobinRunner struct {
	ModeMap          map[string]func() error
	roundRobinConfig *roundRobinConfig
	connections      map[int]client.Client
}

func (r *roundRobinRunner) listenSocket5() error {
	config := r.roundRobinConfig.SOCKS5
	if config.Listen == "" {
		return configError{Field: "listen", Err: errors.New("listen address is empty")}
	}
	l, err := proxymux.ListenSOCKS(config.Listen)
	if err != nil {
		return configError{Field: "listen", Err: err}
	}
	var authFunc func(username, password string) bool
	username, password := config.Username, config.Password
	if username != "" && password != "" {
		authFunc = func(u, p string) bool {
			return u == username && p == password
		}
	}
	var servers []*socks5.Server
	for idx, _ := range r.roundRobinConfig.ClientConfigs {
		c, err := r.ensureConnection(idx)
		if err != nil {
			return err
		}
		s := &socks5.Server{
			HyClient:    c,
			AuthFunc:    authFunc,
			DisableUDP:  config.DisableUDP,
			EventLogger: &socks5Logger{},
		}
		servers = append(servers, s)
	}
	s := socks5.RoundRobinServer{
		Servers:     servers,
		EventLogger: &socks5Logger{},
	}
	logger.Info("SOCKS5 server listening", zap.String("addr", config.Listen))
	return s.Serve(l)
}

func (r *roundRobinRunner) listenHttp() error {
	config := r.roundRobinConfig.HTTP
	if config.Listen == "" {
		return configError{Field: "listen", Err: errors.New("listen address is empty")}
	}
	l, err := proxymux.ListenHTTP(config.Listen)
	if err != nil {
		return configError{Field: "listen", Err: err}
	}
	var authFunc func(username, password string) bool
	username, password := config.Username, config.Password
	if username != "" && password != "" {
		authFunc = func(u, p string) bool {
			return u == username && p == password
		}
	}
	if config.Realm == "" {
		config.Realm = "Hysteria"
	}
	var servers []*http.Server
	for idx, _ := range r.roundRobinConfig.ClientConfigs {
		c, err := r.ensureConnection(idx)
		if err != nil {
			return err
		}
		s := &http.Server{
			HyClient:    c,
			AuthFunc:    authFunc,
			AuthRealm:   config.Realm,
			EventLogger: &httpLogger{},
		}
		servers = append(servers, s)
	}
	s := http.RoundRobinServer{
		Servers:     servers,
		EventLogger: &httpLogger{},
	}
	logger.Info("SOCKS5 server listening", zap.String("addr", config.Listen))
	return s.Serve(l)
}

func (r *roundRobinRunner) Run(errChan chan modeError) {
	if r == nil {
		return
	}
	for name, f := range r.ModeMap {
		go func(name string, f func() error) {
			err := f()
			errChan <- modeError{name, err}
		}(name, f)
	}
}

func (r *roundRobinRunner) Stop() {
	if r == nil {
		return
	}
	for _, c := range r.connections {
		c.Close()
	}
}

func (r *roundRobinRunner) ensureConnection(idx int) (client.Client, error) {
	if c, ok := r.connections[idx]; ok {
		return c, nil
	}
	c, err := client.NewReconnectableClient(
		r.roundRobinConfig.ClientConfigs[idx].Config,
		func(c client.Client, info *client.HandshakeInfo, count int) {
			connectLog(info, count)
			// On the client side, we start checking for updates after we successfully connect
			// to the server, which, depending on whether Lazy mode is enabled, may or may not
			// be immediately after the client starts. We don't want the update check request
			// to interfere with the Lazy mode option.
			if count == 1 && !disableUpdateCheck {
				go runCheckUpdateClient(c)
			}
		}, r.roundRobinConfig.ClientConfigs[idx].Lazy)
	if err != nil {
		return nil, err
	}
	r.connections[idx] = c
	return c, nil
}
