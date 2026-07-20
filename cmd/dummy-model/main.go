package main

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/michaelquigley/df/dl"
	"github.com/michaelquigley/pfxlog"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {
	dl.Init(dl.DefaultOptions().SetTrimPrefix("github.com/openziti/"))
	pfxlog.GlobalInit(logrus.WarnLevel, pfxlog.DefaultOptions().SetTrimPrefix("github.com/openziti/"))
	if err := newRootCommand().cmd.Execute(); err != nil {
		dl.Fatal(err)
	}
}

type rootCommand struct {
	cmd    *cobra.Command
	listen string
	cfg    config
}

func newRootCommand() *rootCommand {
	rc := &rootCommand{}
	rc.cmd = &cobra.Command{
		Use:   "dummy-model",
		Short: "fake OpenAI-compatible backend for testing and demos",
		Long: "dummy-model serves an OpenAI-compatible endpoint backed by a fake model.\n" +
			"it performs no real inference and returns deterministic responses, so it can\n" +
			"stand in as the gateway's local backend for tests and demos.",
		RunE: rc.run,
	}
	rc.cmd.Flags().StringVar(&rc.listen, "listen", ":8081", "listen address")
	rc.cmd.Flags().StringSliceVar(&rc.cfg.models, "model", []string{"dummy"}, "model id(s) advertised by /v1/models (repeatable)")
	rc.cmd.Flags().StringVar(&rc.cfg.response, "response", "", "canned reply text; empty echoes the last user message")
	rc.cmd.Flags().DurationVar(&rc.cfg.streamDelay, "stream-delay", 0, "delay between streamed chunks (e.g. 30ms)")
	rc.cmd.Flags().Float64Var(&rc.cfg.errorRate, "error-rate", 0, "fraction of chat requests to fail [0,1]")
	rc.cmd.Flags().StringVar(&rc.cfg.errorType, "error-type", "rate_limit_error", "OpenAI error type to inject when failing")
	return rc
}

func (rc *rootCommand) run(_ *cobra.Command, _ []string) error {
	srv := newServer(rc.cfg)

	server := &http.Server{
		Addr:    rc.listen,
		Handler: srv.handler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dl.Infof("dummy-model listening on '%s' (models=%v)", rc.listen, srv.cfg.models)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		dl.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
