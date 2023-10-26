package dynamomq

type Result struct {
	ID                   string `json:"id"`
	Status               Status `json:"status"`
	LastUpdatedTimestamp string `json:"last_updated_timestamp"`
	Version              int    `json:"version"`
}

type DequeueResult[T any] struct {
	*Result
	DequeuedMessageObject *Message[T] `json:"-"`
}

// EnqueueResult represents the result for the enqueue() API call.
type EnqueueResult[T any] struct {
	*Result             // Embedded type for inheritance-like behavior in Go
	Message *Message[T] `json:"-"`
}

// PeekResult represents the result for the peek() API call.
type PeekResult[T any] struct {
	*Result                            // Embedded type for inheritance-like behavior in Go
	PeekFromQueueTimestamp string      `json:"queue_peek_timestamp"`
	PeekedMessageObject    *Message[T] `json:"-"`
}

// RetryResult represents the result for the retry() API call.
type RetryResult[T any] struct {
	*Result             // Embedded type for inheritance-like behavior in Go
	Message *Message[T] `json:"-"`
}