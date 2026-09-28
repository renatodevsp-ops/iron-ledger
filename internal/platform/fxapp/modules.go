package fxapp

import "go.uber.org/fx"

// Module is the whole application graph. It is a function so main stays a
// single fx.App call and a test can build the same graph with overrides.
func Module() fx.Option {
	return fx.Module("ironledger",
		fx.Provide(
			ProvideConfig,
			ProvideLogger,
			ProvidePool,
			ProvideVerifier,
			ProvideIDGen,
			ProvideUOW,
			ProvideService,
			ProvideHealthCheck,
			ProvideHTTPServer,
			ProvideListener,
		),
		fx.Invoke(RegisterLifecycle),
	)
}
