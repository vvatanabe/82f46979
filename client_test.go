package dynamomq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	uuid "github.com/satori/go.uuid"
	"github.com/upsidr/dynamotest"
	"github.com/vvatanabe/dynamomq/internal/clock"
	"github.com/vvatanabe/dynamomq/internal/test"
)

type mockClock struct {
	t time.Time
}

func (m mockClock) Now() time.Time {
	return m.t
}

func withClock(clock clock.Clock) func(s *ClientOptions) {
	return func(s *ClientOptions) {
		if clock != nil {
			s.Clock = clock
		}
	}
}

func setupDynamoDB(t *testing.T, initialData ...*types.PutRequest) (tableName string, client *dynamodb.Client, clean func()) {
	client, clean = dynamotest.NewDynamoDB(t)
	tableName = DefaultTableName + "-" + uuid.NewV4().String()
	dynamotest.PrepTable(t, client, dynamotest.InitialTableSetup{
		Table: &dynamodb.CreateTableInput{
			AttributeDefinitions: []types.AttributeDefinition{
				{
					AttributeName: aws.String("id"),
					AttributeType: types.ScalarAttributeTypeS,
				},
				{
					AttributeName: aws.String("queue_type"),
					AttributeType: types.ScalarAttributeTypeS,
				},
				{
					AttributeName: aws.String("queue_add_timestamp"),
					AttributeType: types.ScalarAttributeTypeS,
				},
			},
			BillingMode:               types.BillingModePayPerRequest,
			DeletionProtectionEnabled: aws.Bool(false),
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
				{
					IndexName: aws.String("dynamo-mq-index-queue_type-queue_add_timestamp"),
					KeySchema: []types.KeySchemaElement{
						{
							AttributeName: aws.String("queue_type"),
							KeyType:       types.KeyTypeHash,
						},
						{
							AttributeName: aws.String("queue_add_timestamp"),
							KeyType:       types.KeyTypeRange,
						},
					},
					Projection: &types.Projection{
						ProjectionType: types.ProjectionTypeAll,
					},
				},
			},
			KeySchema: []types.KeySchemaElement{
				{
					AttributeName: aws.String("id"),
					KeyType:       types.KeyTypeHash,
				},
			},
			TableName: aws.String(tableName),
		},
		InitialData: initialData,
	})
	return
}

func newTestMessageItemAsReady(id string, now time.Time) *Message[test.MessageData] {
	return NewMessage[test.MessageData](id, test.NewMessageData(id), now)
}

func newTestMessageItemAsPeeked(id string, now time.Time) *Message[test.MessageData] {
	message := NewMessage[test.MessageData](id, test.NewMessageData(id), now)
	err := message.markAsProcessing(now, 0)
	if err != nil {
		panic(err)
	}
	return message
}

func newTestMessageItemAsDLQ(id string, now time.Time) *Message[test.MessageData] {
	message := NewMessage[test.MessageData](id, test.NewMessageData(id), now)
	err := message.markAsMovedToDLQ(now)
	if err != nil {
		panic(err)
	}
	return message
}

func TestDynamoMQClientSendMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		args     *SendMessageInput[test.MessageData]
		want     *SendMessageOutput[test.MessageData]
		wantErr  error
	}{
		{
			name: "should return IDNotProvidedError",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: &SendMessageInput[test.MessageData]{
				ID:   "",
				Data: test.MessageData{},
			},
			want:    nil,
			wantErr: &IDNotProvidedError{},
		},
		{
			name: "should return IDDuplicatedError",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: &SendMessageInput[test.MessageData]{
				ID:   "A-101",
				Data: test.NewMessageData("A-101"),
			},
			want:    nil,
			wantErr: &IDDuplicatedError{},
		},
		{
			name: "should enqueue succeeds",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t)
			},
			sdkClock: mockClock{
				t: time.Date(2023, 12, 1, 0, 0, 10, 0, time.UTC),
			},
			args: &SendMessageInput[test.MessageData]{
				ID:   "A-101",
				Data: test.NewMessageData("A-101"),
			},
			want: &SendMessageOutput[test.MessageData]{
				Result: &Result{
					ID:                   "A-101",
					Status:               StatusReady,
					LastUpdatedTimestamp: clock.FormatRFC3339Nano(time.Date(2023, 12, 1, 0, 0, 10, 0, time.UTC)),
					Version:              1,
				},
				Message: func() *Message[test.MessageData] {
					s := newTestMessageItemAsReady("A-101", time.Date(2023, 12, 1, 0, 0, 10, 0, time.UTC))
					return s
				}(),
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			cfg, err := config.LoadDefaultConfig(ctx)
			if err != nil {
				t.Fatalf("failed to load aws config: %s\n", err)
				return
			}
			client, err := NewFromConfig[test.MessageData](cfg, WithTableName(tableName), WithAWSDynamoDBClient(raw), withClock(tt.sdkClock))
			if err != nil {
				t.Fatalf("NewFromConfig() error = %v", err)
				return
			}
			result, err := client.SendMessage(ctx, tt.args)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("SendMessage() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				return
			}
			if err != nil {
				t.Errorf("SendMessage() error = %v", err)
				return
			}
			if !reflect.DeepEqual(result, tt.want) {
				t.Errorf("SendMessage() got = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestDynamoMQClientReceiveMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		want     *ReceiveMessageOutput[test.MessageData]
		wantErr  error
	}{
		{
			name: "should return EmptyQueueError",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("A-202", clock.Now()).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsDLQ("A-303", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			want:    nil,
			wantErr: &EmptyQueueError{},
		},
		{
			name: "should peek when not selected",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("B-202",
							time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC)).
							marshalMapUnsafe(),
					},
				)
			},
			sdkClock: mockClock{
				t: time.Date(2023, 12, 1, 0, 0, 10, 0, time.UTC),
			},
			want: func() *ReceiveMessageOutput[test.MessageData] {
				s := newTestMessageItemAsReady("B-202", time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC))
				err := s.markAsProcessing(time.Date(2023, 12, 1, 0, 0, 10, 0, time.UTC), 0)
				if err != nil {
					panic(err)
				}
				s.Version = 2
				s.ReceiveCount = 1
				r := &ReceiveMessageOutput[test.MessageData]{
					Result: &Result{
						ID:                   s.ID,
						Status:               s.Status,
						LastUpdatedTimestamp: s.LastUpdatedTimestamp,
						Version:              s.Version,
					},
					PeekFromQueueTimestamp: s.PeekFromQueueTimestamp,
					PeekedMessageObject:    s,
				}
				return r
			}(),
			wantErr: nil,
		},
		{
			name: "should peek when visibility timeout has expired",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("B-202",
							time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC)).
							marshalMapUnsafe(),
					},
				)
			},
			sdkClock: mockClock{
				t: time.Date(2023, 12, 1, 0, 1, 1, 0, time.UTC),
			},
			want: func() *ReceiveMessageOutput[test.MessageData] {
				s := newTestMessageItemAsPeeked("B-202", time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC))
				err := s.markAsProcessing(time.Date(2023, 12, 1, 0, 1, 1, 0, time.UTC), 0)
				if err != nil {
					panic(err)
				}
				s.Version = 2
				s.ReceiveCount = 1
				r := &ReceiveMessageOutput[test.MessageData]{
					Result: &Result{
						ID:                   s.ID,
						Status:               s.Status,
						LastUpdatedTimestamp: s.LastUpdatedTimestamp,
						Version:              s.Version,
					},
					PeekFromQueueTimestamp: s.PeekFromQueueTimestamp,
					PeekedMessageObject:    s,
				}
				return r
			}(),
			wantErr: nil,
		},
		{
			name: "can not peek when visibility timeout",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("B-202",
							time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC)).
							marshalMapUnsafe(),
					},
				)
			},
			sdkClock: mockClock{
				t: time.Date(2023, 12, 1, 0, 0, 59, 0, time.UTC),
			},
			want:    nil,
			wantErr: &EmptyQueueError{},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			cfg, err := config.LoadDefaultConfig(ctx)
			if err != nil {
				t.Fatalf("failed to load aws config: %s\n", err)
				return
			}
			client, err := NewFromConfig[test.MessageData](cfg, WithTableName(tableName), WithAWSDynamoDBClient(raw), withClock(tt.sdkClock), WithAWSVisibilityTimeout(1))
			if err != nil {
				t.Fatalf("NewFromConfig() error = %v", err)
				return
			}
			result, err := client.ReceiveMessage(ctx, &ReceiveMessageInput{})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("ReceiveMessage() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				return
			}
			if err != nil {
				t.Errorf("ReceiveMessage() error = %v", err)
				return
			}
			if !reflect.DeepEqual(result, tt.want) {
				t.Errorf("ReceiveMessage() got = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestDynamoMQClientReceiveMessageUseFIFO(t *testing.T) {
	t.Parallel()
	tableName, raw, clean := setupDynamoDB(t,
		&types.PutRequest{
			Item: newTestMessageItemAsReady("A-101", time.Date(2023, 12, 1, 0, 0, 3, 0, time.UTC)).marshalMapUnsafe(),
		},
		&types.PutRequest{
			Item: newTestMessageItemAsReady("A-202", time.Date(2023, 12, 1, 0, 0, 2, 0, time.UTC)).marshalMapUnsafe(),
		},
		&types.PutRequest{
			Item: newTestMessageItemAsReady("A-303", time.Date(2023, 12, 1, 0, 0, 1, 0, time.UTC)).marshalMapUnsafe(),
		},
	)
	defer clean()

	now := time.Date(2023, 12, 1, 0, 0, 10, 0, time.UTC)

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("failed to load aws config: %s\n", err)
		return
	}
	client, err := NewFromConfig[test.MessageData](cfg, WithTableName(tableName),
		WithAWSDynamoDBClient(raw),
		withClock(mockClock{
			t: now,
		}),
		WithAWSVisibilityTimeout(1),
		WithUseFIFO(true))
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
		return
	}

	want1 := func() *ReceiveMessageOutput[test.MessageData] {
		s := newTestMessageItemAsReady("A-303", time.Date(2023, 12, 1, 0, 0, 1, 0, time.UTC))
		err := s.markAsProcessing(now, 0)
		if err != nil {
			panic(err)
		}
		s.Version = 2
		s.ReceiveCount = 1
		r := &ReceiveMessageOutput[test.MessageData]{
			Result: &Result{
				ID:                   s.ID,
				Status:               s.Status,
				LastUpdatedTimestamp: s.LastUpdatedTimestamp,
				Version:              s.Version,
			},
			PeekFromQueueTimestamp: s.PeekFromQueueTimestamp,
			PeekedMessageObject:    s,
		}
		return r
	}()

	result, err := client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if err != nil {
		t.Errorf("ReceiveMessage() 1 error = %v", err)
		return
	}
	if !reflect.DeepEqual(result, want1) {
		v1, _ := json.Marshal(result)
		v2, _ := json.Marshal(want1)
		t.Errorf("ReceiveMessage() 1 got = %v, want %v", string(v1), string(v2))
	}
	_, err = client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if !errors.Is(err, &EmptyQueueError{}) {
		t.Errorf("ReceiveMessage() 2 error = %v, wantErr %v", err, &EmptyQueueError{})
		return
	}
	_, err = client.DeleteMessage(ctx, &DeleteMessageInput{
		ID: result.ID,
	})
	if err != nil {
		t.Errorf("Done() 1 error = %v", err)
		return
	}

	want2 := func() *ReceiveMessageOutput[test.MessageData] {
		s := newTestMessageItemAsReady("A-202", time.Date(2023, 12, 1, 0, 0, 2, 0, time.UTC))
		err := s.markAsProcessing(now, 0)
		if err != nil {
			panic(err)
		}
		s.Version = 2
		s.ReceiveCount = 1
		r := &ReceiveMessageOutput[test.MessageData]{
			Result: &Result{
				ID:                   s.ID,
				Status:               s.Status,
				LastUpdatedTimestamp: s.LastUpdatedTimestamp,
				Version:              s.Version,
			},
			PeekFromQueueTimestamp: s.PeekFromQueueTimestamp,
			PeekedMessageObject:    s,
		}
		return r
	}()

	result, err = client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if err != nil {
		t.Errorf("ReceiveMessage() 3 error = %v", err)
		return
	}
	if !reflect.DeepEqual(result, want2) {
		v1, _ := json.Marshal(result)
		v2, _ := json.Marshal(want2)
		t.Errorf("ReceiveMessage() 3 got = %v, want %v", string(v1), string(v2))
	}

	_, err = client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if !errors.Is(err, &EmptyQueueError{}) {
		t.Errorf("ReceiveMessage() 4 error = %v, wantErr %v", err, &EmptyQueueError{})
		return
	}
	_, err = client.DeleteMessage(ctx, &DeleteMessageInput{
		ID: result.ID,
	})
	if err != nil {
		t.Errorf("Done() 2 error = %v", err)
		return
	}

	want3 := func() *ReceiveMessageOutput[test.MessageData] {
		s := newTestMessageItemAsReady("A-101", time.Date(2023, 12, 1, 0, 0, 3, 0, time.UTC))
		err := s.markAsProcessing(now, 0)
		if err != nil {
			panic(err)
		}
		s.Version = 2
		s.ReceiveCount = 1
		r := &ReceiveMessageOutput[test.MessageData]{
			Result: &Result{
				ID:                   s.ID,
				Status:               s.Status,
				LastUpdatedTimestamp: s.LastUpdatedTimestamp,
				Version:              s.Version,
			},
			PeekFromQueueTimestamp: s.PeekFromQueueTimestamp,
			PeekedMessageObject:    s,
		}
		return r
	}()

	result, err = client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if err != nil {
		t.Errorf("ReceiveMessage() 5 error = %v", err)
		return
	}
	if !reflect.DeepEqual(result, want3) {
		v1, _ := json.Marshal(result)
		v2, _ := json.Marshal(want3)
		t.Errorf("ReceiveMessage() 5 got = %v, want %v", string(v1), string(v2))
	}

	_, err = client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if !errors.Is(err, &EmptyQueueError{}) {
		t.Errorf("ReceiveMessage() 6 error = %v, wantErr %v", err, &EmptyQueueError{})
		return
	}
	_, err = client.DeleteMessage(ctx, &DeleteMessageInput{
		ID: result.ID,
	})
	if err != nil {
		t.Errorf("Done() 3 error = %v", err)
		return
	}
	_, err = client.ReceiveMessage(ctx, &ReceiveMessageInput{})
	if !errors.Is(err, &EmptyQueueError{}) {
		t.Errorf("ReceiveMessage() 7 error = %v, wantErr %v", err, &EmptyQueueError{})
		return
	}
}

func TestDynamoMQClientReceiveMessageNotUseFIFO(t *testing.T) {
	t.Parallel()
	tableName, raw, clean := setupDynamoDB(t,
		&types.PutRequest{
			Item: newTestMessageItemAsReady("A-101", date(2023, 12, 1, 0, 0, 3)).
				marshalMapUnsafe(),
		},
		&types.PutRequest{
			Item: newTestMessageItemAsReady("A-202", date(2023, 12, 1, 0, 0, 2)).
				marshalMapUnsafe(),
		},
		&types.PutRequest{
			Item: newTestMessageItemAsReady("A-303", date(2023, 12, 1, 0, 0, 1)).
				marshalMapUnsafe(),
		},
	)
	defer clean()

	now := date(2023, 12, 1, 0, 0, 10)
	optFns := []func(*ClientOptions){
		WithTableName(tableName),
		WithAWSDynamoDBClient(raw),
		withClock(mockClock{
			t: now,
		}),
		WithAWSVisibilityTimeout(1),
	}

	newTestMessage := func(id string, before time.Time, after time.Time) *ReceiveMessageOutput[test.MessageData] {
		s := newTestMessageItemAsReady(id, before)
		_ = s.markAsProcessing(after, 0)
		s.Version = 2
		s.ReceiveCount = 1
		r := &ReceiveMessageOutput[test.MessageData]{
			Result: &Result{
				ID:                   s.ID,
				Status:               s.Status,
				LastUpdatedTimestamp: s.LastUpdatedTimestamp,
				Version:              s.Version,
			},
			PeekFromQueueTimestamp: s.PeekFromQueueTimestamp,
			PeekedMessageObject:    s,
		}
		return r
	}

	wants := []*ReceiveMessageOutput[test.MessageData]{
		newTestMessage("A-303", date(2023, 12, 1, 0, 0, 1), now),
		newTestMessage("A-202", date(2023, 12, 1, 0, 0, 2), now),
		newTestMessage("A-101", date(2023, 12, 1, 0, 0, 3), now),
	}

	ctx := context.Background()
	handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
		for i, want := range wants {
			result, err := client.ReceiveMessage(ctx, &ReceiveMessageInput{})
			if err != nil {
				t.Errorf("ReceiveMessage() [%d] error = %v", i, err)
				return
			}
			if !reflect.DeepEqual(result, want) {
				v1, _ := json.Marshal(result)
				v2, _ := json.Marshal(want)
				t.Errorf("ReceiveMessage() [%d] got = %v, want %v", i, string(v1), string(v2))
			}
		}
		_, err := client.ReceiveMessage(ctx, &ReceiveMessageInput{})
		if !errors.Is(err, &EmptyQueueError{}) {
			t.Errorf("ReceiveMessage() [last] error = %v, wantErr %v", err, &EmptyQueueError{})
			return
		}
	})
}

func TestDynamoMQClientUpdateMessageAsVisible(t *testing.T) {
	t.Parallel()
	type args struct {
		id string
	}
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		args     args
		want     *UpdateMessageAsVisibleOutput[test.MessageData]
		wantErr  error
	}{
		{
			name: "should return IDNotProvidedError when id is empty",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "",
			},
			want:    nil,
			wantErr: &IDNotProvidedError{},
		},
		{
			name: "should return IDNotFoundError when id is not found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "B-202",
			},
			want:    nil,
			wantErr: &IDNotFoundError{},
		},
		{
			name: "should succeed when id is found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("A-101",
							date(2023, 12, 1, 0, 0, 10)).marshalMapUnsafe(),
					},
				)
			},
			sdkClock: mockClock{
				t: date(2023, 12, 1, 0, 0, 10),
			},
			args: args{
				id: "A-101",
			},
			want: &UpdateMessageAsVisibleOutput[test.MessageData]{
				Result: &Result{
					ID:     "A-101",
					Status: StatusReady,
					LastUpdatedTimestamp: clock.FormatRFC3339Nano(
						date(2023, 12, 1, 0, 0, 10)),
					Version: 2,
				},
				Message: func() *Message[test.MessageData] {
					now := date(2023, 12, 1, 0, 0, 10)
					message := newTestMessageItemAsPeeked("A-101", now)
					err := message.markAsReady(now)
					if err != nil {
						panic(err)
					}
					message.Version = 2
					return message
				}(),
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				withClock(tt.sdkClock),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				result, err := client.UpdateMessageAsVisible(ctx, &UpdateMessageAsVisibleInput{
					ID: tt.args.id,
				})
				err = checkExpectedError(t, err, tt.wantErr, "UpdateMessageAsVisible()")
				if err != nil || tt.wantErr != nil {
					return
				}
				if !reflect.DeepEqual(result, tt.want) {
					t.Errorf("UpdateMessageAsVisible() got = %v, want %v", result, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientDeleteMessage(t *testing.T) {
	t.Parallel()
	type args struct {
		id string
	}
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		args     args
		want     error
	}{
		{
			name: "should return IDNotProvidedError when id is empty",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "",
			},
			want: &IDNotProvidedError{},
		},
		{
			name: "should not return error when not existing id",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "B-101",
			},
			want: nil,
		},
		{
			name: "should succeed when id is found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "A-101",
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				withClock(tt.sdkClock),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				_, err := client.DeleteMessage(ctx, &DeleteMessageInput{
					ID: tt.args.id,
				})
				err = checkExpectedError(t, err, tt.want, "DeleteMessage()")
			})
		})
	}
}

func TestDynamoMQClientMoveMessageToDLQ(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		args     *MoveMessageToDLQInput
		want     *MoveMessageToDLQOutput
		wantErr  error
	}{
		{
			name: "should return IDNotProvidedError when id is empty",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: &MoveMessageToDLQInput{
				ID: "",
			},
			want:    nil,
			wantErr: &IDNotProvidedError{},
		},
		{
			name: "should return IDNotFoundError when id is not found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: &MoveMessageToDLQInput{
				ID: "B-202",
			},
			want:    nil,
			wantErr: &IDNotFoundError{},
		},
		{
			name: "should succeed when id is found and queue type is standard",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsDLQ("A-101",
							date(2023, 12, 1, 0, 0, 0)).
							marshalMapUnsafe(),
					},
				)
			},
			args: &MoveMessageToDLQInput{
				ID: "A-101",
			},
			want: func() *MoveMessageToDLQOutput {
				s := newTestMessageItemAsDLQ("A-101",
					date(2023, 12, 1, 0, 0, 0))
				r := &MoveMessageToDLQOutput{
					ID:                   s.ID,
					Status:               s.Status,
					LastUpdatedTimestamp: s.LastUpdatedTimestamp,
					Version:              s.Version,
				}
				return r
			}(),
			wantErr: nil,
		},
		{
			name: "should succeed when id is found and queue type is DLQ and status is processing",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("A-101",
							date(2023, 12, 1, 0, 0, 0)).
							marshalMapUnsafe(),
					},
				)
			},
			sdkClock: mockClock{
				t: date(2023, 12, 1, 0, 0, 10),
			},
			args: &MoveMessageToDLQInput{
				ID: "A-101",
			},
			want: func() *MoveMessageToDLQOutput {
				s := newTestMessageItemAsReady("A-101",
					date(2023, 12, 1, 0, 0, 0))
				err := s.markAsMovedToDLQ(date(2023, 12, 1, 0, 0, 10))
				if err != nil {
					panic(err)
				}
				s.Version = 2
				r := &MoveMessageToDLQOutput{
					ID:                   s.ID,
					Status:               s.Status,
					LastUpdatedTimestamp: s.LastUpdatedTimestamp,
					Version:              s.Version,
				}
				return r
			}(),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				withClock(tt.sdkClock),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				result, err := client.MoveMessageToDLQ(ctx, tt.args)
				err = checkExpectedError(t, err, tt.wantErr, "MoveMessageToDLQ()")
				if err != nil || tt.wantErr != nil {
					return
				}
				if !reflect.DeepEqual(result, tt.want) {
					t.Errorf("MoveMessageToDLQ() got = %v, want %v", result, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientRedriveMessage(t *testing.T) {
	t.Parallel()
	type args struct {
		id string
	}
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		args     args
		want     *RedriveMessageOutput
		wantErr  error
	}{
		{
			name: "should return IDNotProvidedError when id is empty",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "",
			},
			want:    nil,
			wantErr: &IDNotProvidedError{},
		},
		{
			name: "should return IDNotFoundError when id is not found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "B-202",
			},
			want:    nil,
			wantErr: &IDNotFoundError{},
		},
		{
			name: "should succeed when id is found and status is ready",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsDLQ("A-101",
							date(2023, 12, 1, 0, 0, 0)).marshalMapUnsafe(),
					},
				)
			},
			sdkClock: mockClock{
				t: date(2023, 12, 1, 0, 0, 10),
			},
			args: args{
				id: "A-101",
			},
			want: &RedriveMessageOutput{
				ID:     "A-101",
				Status: StatusReady,
				LastUpdatedTimestamp: clock.FormatRFC3339Nano(
					date(2023, 12, 1, 0, 0, 10)),
				Version: 2,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				withClock(tt.sdkClock),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				result, err := client.RedriveMessage(ctx, &RedriveMessageInput{
					ID: tt.args.id,
				})
				err = checkExpectedError(t, err, tt.wantErr, "RedriveMessage()")
				if err != nil || tt.wantErr != nil {
					return
				}
				if !reflect.DeepEqual(result, tt.want) {
					t.Errorf("RedriveMessage() got = %v, want %v", result, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientGetQueueStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(*testing.T) (string, *dynamodb.Client, func())
		want    *GetQueueStatsOutput
		wantErr error
	}{
		{
			name: "should return empty items stats when no item in standard queue",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t)
			},
			want: &GetQueueStatsOutput{
				First100IDsInQueue:         []string{},
				First100SelectedIDsInQueue: []string{},
				TotalRecordsInQueue:        0,
				TotalRecordsInProcessing:   0,
				TotalRecordsNotStarted:     0,
			},
		},
		{
			name: "should return one item stats when one item in standard queue",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			want: &GetQueueStatsOutput{
				First100IDsInQueue:         []string{"A-101"},
				First100SelectedIDsInQueue: []string{},
				TotalRecordsInQueue:        1,
				TotalRecordsInProcessing:   0,
				TotalRecordsNotStarted:     1,
			},
		},
		{
			name: "should return one processing item stats when one item in standard queue",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			want: &GetQueueStatsOutput{
				First100IDsInQueue:         []string{"A-101"},
				First100SelectedIDsInQueue: []string{"A-101"},
				TotalRecordsInQueue:        1,
				TotalRecordsInProcessing:   1,
				TotalRecordsNotStarted:     0,
			},
		},
		{
			name: "should return two items stats when two items in standard queue",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsReady("B-202", clock.Now().Add(1*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("C-303", clock.Now().Add(2*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("D-404", clock.Now().Add(3*time.Second)).marshalMapUnsafe(),
					},
				)
			},
			want: &GetQueueStatsOutput{
				First100IDsInQueue:         []string{"A-101", "B-202", "C-303", "D-404"},
				First100SelectedIDsInQueue: []string{"C-303", "D-404"},
				TotalRecordsInQueue:        4,
				TotalRecordsInProcessing:   2,
				TotalRecordsNotStarted:     2,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				got, err := client.GetQueueStats(ctx, &GetQueueStatsInput{})
				err = checkExpectedError(t, err, tt.wantErr, "GetQueueStats()")
				if err != nil || tt.wantErr != nil {
					return
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("GetQueueStats() got = %v, want %v", got, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientGetDLQStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(*testing.T) (string, *dynamodb.Client, func())
		want    *GetDLQStatsOutput
		wantErr error
	}{
		{
			name: "should return empty items when no items in DLQ",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now().Add(time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsReady("B-202", clock.Now().Add(1*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("C-303", clock.Now().Add(2*time.Second)).marshalMapUnsafe(),
					},
				)
			},
			want: &GetDLQStatsOutput{
				First100IDsInQueue: []string{},
				TotalRecordsInDLQ:  0,
			},
		},
		{
			name: "should return three DLQ items when items in DLQ",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsReady("B-202", clock.Now().Add(1*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsPeeked("C-303", clock.Now().Add(2*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsDLQ("D-404", clock.Now().Add(3*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsDLQ("E-505", clock.Now().Add(4*time.Second)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsDLQ("F-606", clock.Now().Add(5*time.Second)).marshalMapUnsafe(),
					},
				)
			},
			want: &GetDLQStatsOutput{
				First100IDsInQueue: []string{"D-404", "E-505", "F-606"},
				TotalRecordsInDLQ:  3,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				got, err := client.GetDLQStats(ctx, &GetDLQStatsInput{})
				err = checkExpectedError(t, err, tt.wantErr, "GetDLQStats()")
				if err != nil || tt.wantErr != nil {
					return
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("GetDLQStats() got = %v, want %v", got, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientGetMessage(t *testing.T) {
	t.Parallel()
	type args struct {
		id string
	}
	tests := []struct {
		name    string
		setup   func(*testing.T) (string, *dynamodb.Client, func())
		args    args
		want    *Message[test.MessageData]
		wantErr error
	}{
		{
			name: "should return IDNotProvidedError when id is empty",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "",
			},
			want:    nil,
			wantErr: &IDNotProvidedError{},
		},
		{
			name: "should not return message when id is not found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "B-202",
			},
			want:    nil,
			wantErr: nil,
		},
		{
			name: "should return message when id is found",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101",
							date(2023, 12, 1, 0, 0, 0)).marshalMapUnsafe(),
					},
					&types.PutRequest{
						Item: newTestMessageItemAsReady("B-202", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				id: "A-101",
			},
			want: newTestMessageItemAsReady("A-101",
				date(2023, 12, 1, 0, 0, 0)),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				got, err := client.GetMessage(ctx, &GetMessageInput{
					ID: tt.args.id,
				})
				err = checkExpectedError(t, err, tt.wantErr, "GetMessage()")
				if err != nil || tt.wantErr != nil {
					return
				}
				if !reflect.DeepEqual(got.Message, tt.want) {
					t.Errorf("GetMessage() got = %v, want %v", got.Message, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientReplaceMessage(t *testing.T) {
	t.Parallel()
	type args struct {
		message *Message[test.MessageData]
	}
	tests := []struct {
		name    string
		setup   func(*testing.T) (string, *dynamodb.Client, func())
		args    args
		want    *Message[test.MessageData]
		wantErr error
	}{
		{
			name: "should return IDNotProvidedError when id is empty",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				message: &Message[test.MessageData]{
					ID: "",
				},
			},
			want:    nil,
			wantErr: &IDNotProvidedError{},
		},
		{
			name: "should return message when id is duplicated",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				message: newTestMessageItemAsReady("A-101",
					date(2023, 12, 1, 0, 0, 0)),
			},
			want: newTestMessageItemAsReady("A-101",
				date(2023, 12, 1, 0, 0, 0)),
			wantErr: nil,
		},
		{
			name: "should return message when id is unique",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t,
					&types.PutRequest{
						Item: newTestMessageItemAsReady("A-101", clock.Now()).marshalMapUnsafe(),
					},
				)
			},
			args: args{
				message: newTestMessageItemAsReady("B-202",
					date(2023, 12, 1, 0, 0, 0)),
			},
			want: newTestMessageItemAsReady("B-202",
				date(2023, 12, 1, 0, 0, 0)),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				_, err := client.ReplaceMessage(ctx, &ReplaceMessageInput[test.MessageData]{
					Message: tt.args.message,
				})
				err = checkExpectedError(t, err, tt.wantErr, "ReplaceMessage()")
				if err != nil || tt.wantErr != nil {
					return
				}
				got, err := client.GetMessage(ctx, &GetMessageInput{
					ID: tt.args.message.ID,
				})
				if err != nil {
					t.Errorf("GetMessage() error = %v", err)
					return
				}
				if !reflect.DeepEqual(got.Message, tt.want) {
					t.Errorf("GetMessage() got = %v, want %v", got.Message, tt.want)
				}
			})
		})
	}
}

func TestDynamoMQClientListMessages(t *testing.T) {
	t.Parallel()
	type args struct {
		size int32
	}
	tests := []struct {
		name     string
		setup    func(*testing.T) (string, *dynamodb.Client, func())
		sdkClock clock.Clock
		args     args
		want     []*Message[test.MessageData]
		wantErr  error
	}{
		{
			name: "should return empty list when no messages",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				return setupDynamoDB(t)
			},
			args: args{
				size: 10,
			},
			want:    []*Message[test.MessageData]{},
			wantErr: nil,
		},
		{
			name: "should return list of messages when messages exist",
			setup: func(t *testing.T) (string, *dynamodb.Client, func()) {
				messages := generateExpectedMessages("A",
					date(2023, 12, 1, 0, 0, 0), 10)
				puts := generatePutRequests(messages)
				return setupDynamoDB(t, puts...)
			},
			args: args{
				size: 10,
			},
			want: generateExpectedMessages("A",
				date(2023, 12, 1, 0, 0, 0), 10),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tableName, raw, clean := tt.setup(t)
			defer clean()
			ctx := context.Background()
			optFns := []func(*ClientOptions){
				WithTableName(tableName),
				WithAWSDynamoDBClient(raw),
				withClock(tt.sdkClock),
				WithAWSVisibilityTimeout(1),
			}
			handleTestOperation(t, ctx, optFns, func(client Client[test.MessageData]) {
				result, err := client.ListMessages(ctx, &ListMessagesInput{
					Size: tt.args.size,
				})
				err = checkExpectedError(t, err, tt.wantErr, "ListMessages()")
				if err != nil || tt.wantErr != nil {
					return
				}
				sort.Slice(result.Messages, func(i, j int) bool {
					return result.Messages[i].LastUpdatedTimestamp < result.Messages[j].LastUpdatedTimestamp
				})
				if !reflect.DeepEqual(result.Messages, tt.want) {
					t.Errorf("ListMessages() got = %v, want %v", result, tt.want)
				}
			})
		})
	}
}

func generateExpectedMessages(idPrefix string, now time.Time, count int) []*Message[test.MessageData] {
	messages := make([]*Message[test.MessageData], count)
	for i := 0; i < count; i++ {
		now = now.Add(time.Minute)
		messages[i] = newTestMessageItemAsReady(fmt.Sprintf("%s-%d", idPrefix, i), now)
	}
	return messages
}

func generatePutRequests(messages []*Message[test.MessageData]) []*types.PutRequest {
	var puts []*types.PutRequest
	for _, message := range messages {
		puts = append(puts, &types.PutRequest{
			Item: message.marshalMapUnsafe(),
		})
	}
	return puts
}

func checkExpectedError(t *testing.T, err, wantErr error, messagePrefix string) error {
	if wantErr != nil {
		if !errors.Is(err, wantErr) {
			t.Errorf("%s error = %v, wantErr %v", messagePrefix, err, wantErr)
			return err
		}
		return nil
	}
	if err != nil {
		t.Errorf("%s unexpected error = %v", messagePrefix, err)
		return err
	}
	return nil
}

func handleTestOperation(t *testing.T, ctx context.Context, optFns []func(*ClientOptions), operation func(Client[test.MessageData])) {
	client, err := setupClient(ctx, optFns...)
	if err != nil {
		t.Fatalf("failed to setup client: %s\n", err)
		return
	}
	operation(client)
}

func setupClient(ctx context.Context, optFns ...func(*ClientOptions)) (Client[test.MessageData], error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return NewFromConfig[test.MessageData](cfg, optFns...)
}

func date(year int, month time.Month, day, hour, min, sec int) time.Time {
	return time.Date(year, month, day, hour, min, sec, 0, time.UTC)
}
