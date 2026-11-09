// Command raven-factory is Raven: a factory that builds PowerShell scripts,
// minions that check in through the HTTP channel and do hello and echo. It
// exists to run core's codec mechanics end to end: its minions speak
// PowerShell's CLIXML (raven.clixml), which Raven translates for core over
// the factory link, and its suite, raven.raven, seals nothing (codec.go).
package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-module-core/pkg/lifecycle"
	"github.com/logoscore/logos-golang-module-core/pkg/logging"
	"github.com/logoscore/logos-golang-module-core/pkg/logstream"
	"github.com/logoscore/logos-golang-module-core/pkg/module"
)

// version is reported at registration and goes into the scripts' hello; set
// with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	instance := env("FACTORY_ID", "raven-local")

	// Logs go to stderr (LOG_FORMAT) at the managed log_level, and to core's
	// live log tail through the hub.
	var level slog.LevelVar
	logs := logstream.New(instance)
	logger, err := logging.New(os.Getenv("LOG_FORMAT"), os.Stderr, &level, logs)
	if err != nil {
		log.Fatalf("logging: %v", err)
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("logos-raven starting", "instance", instance, "version", version)
	mgmtSrv, err := newManagement(&level)
	if err != nil {
		fatal("management", "err", err)
	}
	schema := buildSchema
	err = factory.Run(ctx, factory.Config{
		Config: module.Config{
			AMQPURL: amqpURL(),
			Identity: lifecycle.Identity{
				ModuleName:  "raven",
				Instance:    instance,
				Version:     version,
				Description: "Raven: builds PowerShell scripts that check in through the HTTP channel, speaking CLIXML Raven translates for core (a codec test, sealing nothing)",
			},
			Management: mgmtSrv,
			Logs:       logs,
		},
		Build:               &schema,
		Builder:             &raven{version: version},
		MaxConcurrentBuilds: 4,
		// Core calls Raven for every message of its builds' minions: the
		// factory link opens on core's own URL, not through a front.
		Codec:   ravenCodec{},
		CoreURL: env("LOGOS_CORE_BASE_URL", "http://localhost:8002"),
	})
	if err != nil {
		fatal("control plane failed", "err", err)
	}
	slog.Info("bye")
}

// fatal logs at error level and exits.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func amqpURL() string {
	u := url.URL{
		Scheme: "amqp",
		User:   url.UserPassword(env("RABBITMQ_USER", "guest"), env("RABBITMQ_PASSWORD", "guest")),
		Host:   net.JoinHostPort(env("RABBITMQ_HOST", "localhost"), env("RABBITMQ_PORT", "5672")),
		// amqp091 takes the vhost from the path minus its leading "/".
		Path: "/" + env("RABBITMQ_VHOST", "/"),
	}
	return u.String()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
