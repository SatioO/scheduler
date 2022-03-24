package command

import (
	"context"
	"errors"

	"github.com/satioO/scheduler/scheduler/cqrs/marshaler"
	"github.com/sirupsen/logrus"
)

type CommandBus struct {
	marshaler     marshaler.CommandEventMarshaler
	generateTopic func(commandName string) string
}

func NewCommandBus(generateTopic func(commandName string) string, marshaler marshaler.CommandEventMarshaler) (*CommandBus, error) {
	if marshaler == nil {
		return nil, errors.New("missing marshaler")
	}

	if generateTopic == nil {
		return nil, errors.New("missing generateTopic")
	}

	return &CommandBus{
		marshaler,
		generateTopic,
	}, nil
}

func (c CommandBus) Send(ctx context.Context, cmd any) error {
	msg, err := c.marshaler.Marshal(cmd)
	if err != nil {
		return err
	}

	commandName := c.marshaler.Name(cmd)
	topicName := c.generateTopic(commandName)

	msg.SetContext(ctx)

	logrus.Printf("Send::: %v", topicName)
	return nil
}
