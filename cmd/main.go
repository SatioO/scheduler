package main

import (
	"context"

	"github.com/satioO/scheduler/scheduler/cqrs"
	"github.com/satioO/scheduler/scheduler/cqrs/commands"
	"github.com/satioO/scheduler/scheduler/cqrs/marshaler"
	"github.com/satioO/scheduler/scheduler/cqrs/message"
	framework "github.com/satioO/scheduler/scheduler/internal/adapters/framework/rest"
	command "github.com/satioO/scheduler/scheduler/internal/application/command/account"
	"github.com/satioO/scheduler/scheduler/kafka"
)

func main() {
	router, err := message.NewRouter()
	publisher, err := kafka.NewPublisher()

	if err != nil {
		panic(err)
	}

	commandsSubscriber, err := kafka.NewSubscriber()
	if err != nil {
		panic(err)
	}

	config := cqrs.AppConfig{
		GenerateCommandsTopic: func(commandName string) string {
			return commandName
		},
		CommandsPublisher: publisher,
		CommandsSubscriber: func(handlerName string) (message.Subscriber, error) {
			return commandsSubscriber, nil
		},
		CommandHandlers: func(cb *commands.CommandBus) []commands.CommandHandler {
			return []commands.CommandHandler{
				command.OpenAccountHandler{},
				command.CloseAccountHandler{},
			}
		},
		CommandEventMarshaler: marshaler.JSONMarshaler{},
		Router:                router,
	}

	app, err := cqrs.NewApp(&config)

	if err != nil {
		panic(err)
	}

	// processors are based on router, so they will work when router will start
	if err := router.Run(context.Background()); err != nil {
		panic(err)
	}

	// DONE::: HTTP Adapter
	restAdapter := framework.NewHttpServer(app)
	framework.Run(restAdapter)

	// TODO::: GRPC Adapter
	// grpcAdapter := framework.NewGRPCServer(app)
	// framework.Run(grpcAdapter)
}
