package dynamomq

import (
	"context"

	uuid "github.com/satori/go.uuid"
)

func NewProducer[T any](client Client[T]) *Producer[T] {
	return &Producer[T]{
		client:        client,
		uuidGenerator: uuid.NewV4().String,
	}
}

type Producer[T any] struct {
	client        Client[T]
	uuidGenerator func() string
}

type ProduceInput[T any] struct {
	Data T
}

type ProduceOutput[T any] struct {
	Message *Message[T]
}

func (c *Producer[T]) Produce(ctx context.Context, params *ProduceInput[T]) (*ProduceOutput[T], error) {
	if params == nil {
		params = &ProduceInput[T]{}
	}
	out, err := c.client.SendMessage(ctx, &SendMessageInput[T]{
		ID:   c.uuidGenerator(),
		Data: params.Data,
	})
	if err != nil {
		return &ProduceOutput[T]{}, err
	}
	return &ProduceOutput[T]{
		Message: out.Message,
	}, nil
}
