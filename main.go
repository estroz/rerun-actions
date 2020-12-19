package main

import (
	"os"
	"regexp"
	"time"

	"github.com/gregjones/httpcache"
	"github.com/palantir/go-baseapp/baseapp"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/rs/zerolog"
	"goji.io/v3/pat"
)

func main() {
	config, err := readConfig("config.yml")
	if err != nil {
		panic(err)
	}

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	handler := &RerunActionsHandler{}
	for _, reStr := range config.AllowUserRegexpList {
		re, err := regexp.Compile(reStr)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to parse allow user regexp")
			os.Exit(1)
		}
		handler.allowUserRegexps = append(handler.allowUserRegexps, re)
	}
	for _, reStr := range config.DenyUserRegexpList {
		re, err := regexp.Compile(reStr)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to parse deny user regexp")
			os.Exit(1)
		}
		handler.denyUserRegexps = append(handler.denyUserRegexps, re)
	}

	server, err := baseapp.NewServer(
		config.Server,
		baseapp.DefaultParams(logger, "rerun-actions.")...,
	)
	if err != nil {
		panic(err)
	}

	handler.ClientCreator, err = githubapp.NewDefaultCachingClientCreator(
		config.Github,
		githubapp.WithClientUserAgent("rerun-actions-app/0.1.0"),
		githubapp.WithClientTimeout(3*time.Second),
		githubapp.WithClientCaching(false, func() httpcache.Cache { return httpcache.NewMemoryCache() }),
		githubapp.WithClientMiddleware(
			githubapp.ClientMetrics(server.Registry()),
		),
	)
	if err != nil {
		panic(err)
	}

	webhookHandler := githubapp.NewDefaultEventDispatcher(config.Github, handler)
	server.Mux().Handle(pat.Post(githubapp.DefaultWebhookRoute), webhookHandler)

	// Start is blocking
	err = server.Start()
	if err != nil {
		panic(err)
	}
}
