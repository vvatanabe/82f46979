package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/vvatanabe/dynamomq/internal/clock"
)

const (
	AwsRegionDefault                  = "us-east-1"
	AwsProfileDefault                 = "default"
	DefaultTableName                  = "Shipment"
	QueueingIndexName                 = "queueud-last_updated_timestamp-index"
	DlqQueueingIndexName              = "dlq-last_updated_timestamp-index"
	DefaultRetryMaxAttempts           = 10
	DefaultVisibilityTimeoutInMinutes = 1
)

type QueueSDKClient interface {
	GetQueueStats(ctx context.Context) (*QueueStats, error)
	GetDLQStats(ctx context.Context) (*DLQStats, error)
	Get(ctx context.Context, id string) (*Shipment, error)
	Put(ctx context.Context, shipment *Shipment) error
	Upsert(ctx context.Context, shipment *Shipment) error
	UpdateStatus(ctx context.Context, id string, newStatus Status) (*Result, error)
	Enqueue(ctx context.Context, id string) (*EnqueueResult, error)
	Peek(ctx context.Context) (*PeekResult, error)
	Dequeue(ctx context.Context) (*DequeueResult, error)
	Remove(ctx context.Context, id string) (*Result, error)
	Restore(ctx context.Context, id string) (*Result, error)
	SendToDLQ(ctx context.Context, id string) (*Result, error)
	Touch(ctx context.Context, id string) (*Result, error)
	List(ctx context.Context, size int32) ([]*Shipment, error)
	ListIDs(ctx context.Context, size int32) ([]string, error)
	ListExtendedIDs(ctx context.Context, size int32) ([]string, error)
	Delete(ctx context.Context, id string) error
	CreateTestData(ctx context.Context, id string) (*Shipment, error)
}

type queueSDKClient struct {
	dynamoDB *dynamodb.Client

	tableName                 string
	awsRegion                 string
	awsCredentialsProfileName string
	baseEndpoint              string
	credentialsProvider       aws.CredentialsProvider

	retryMaxAttempts           int
	visibilityTimeoutInMinutes int

	clock clock.Clock
}

type Option func(*queueSDKClient)

func WithTableName(tableName string) Option {
	return func(s *queueSDKClient) {
		s.tableName = tableName
	}
}

func WithAWSRegion(awsRegion string) Option {
	return func(s *queueSDKClient) {
		s.awsRegion = awsRegion
	}
}

func WithAWSCredentialsProfileName(awsCredentialsProfileName string) Option {
	return func(s *queueSDKClient) {
		s.awsCredentialsProfileName = awsCredentialsProfileName
	}
}

func WithAWSCredentialsProvider(credentialsProvider aws.CredentialsProvider) Option {
	return func(s *queueSDKClient) {
		s.credentialsProvider = credentialsProvider
	}
}

func WithAWSBaseEndpoint(baseEndpoint string) Option {
	return func(s *queueSDKClient) {
		s.baseEndpoint = baseEndpoint
	}
}

func WithAWSRetryMaxAttempts(retryMaxAttempts int) Option {
	return func(s *queueSDKClient) {
		s.retryMaxAttempts = retryMaxAttempts
	}
}

func WithAWSVisibilityTimeout(minutes int) Option {
	return func(s *queueSDKClient) {
		s.visibilityTimeoutInMinutes = minutes
	}
}

func WithAWSDynamoDBClient(client *dynamodb.Client) Option {
	return func(s *queueSDKClient) {
		s.dynamoDB = client
	}
}

func NewQueueSDKClient(ctx context.Context, opts ...Option) (QueueSDKClient, error) {
	c := &queueSDKClient{
		tableName:                  DefaultTableName,
		awsRegion:                  AwsRegionDefault,
		awsCredentialsProfileName:  AwsProfileDefault,
		retryMaxAttempts:           DefaultRetryMaxAttempts,
		visibilityTimeoutInMinutes: DefaultVisibilityTimeoutInMinutes,
		clock:                      &clock.RealClock{},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.dynamoDB != nil {
		return c, nil
	}
	if c.credentialsProvider == nil {
		accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
		secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
		sessionToken := os.Getenv("AWS_SESSION_TOKEN")
		creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)
		c.credentialsProvider = &creds
	}
	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(c.awsRegion),
		config.WithCredentialsProvider(c.credentialsProvider),
		config.WithSharedConfigProfile(c.awsCredentialsProfileName),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load aws config: %w", err)
	}
	c.dynamoDB = dynamodb.NewFromConfig(cfg, func(options *dynamodb.Options) {
		options.RetryMaxAttempts = c.retryMaxAttempts
		if c.baseEndpoint != "" {
			options.BaseEndpoint = aws.String(c.baseEndpoint)
		}
	})
	return c, nil
}

// GetQueueStats retrieves statistics about the current state of the queue.
//
// The function queries the DynamoDB table to fetch records that are currently queued.
// It calculates the total number of records in the queue, the number of records that are
// currently being processed, and the number of records that have not yet started processing.
// Additionally, it provides the IDs of the first 100 records in the queue and the IDs of the
// first 100 records that are currently being processed.
//
// Parameters:
//
//	ctx: The context for the request, used for timeout and cancellation.
//
// Returns:
//   - A pointer to a QueueStats object containing the calculated statistics.
//   - An error if there's any issue querying the database or processing the results.
//
// Note: The function uses pagination to query the DynamoDB table and will continue querying
// until all records have been fetched or an error occurs.
func (c *queueSDKClient) GetQueueStats(ctx context.Context) (*QueueStats, error) {
	expr, err := expression.NewBuilder().
		WithProjection(expression.NamesList(expression.Name("id"), expression.Name("system_info"))).
		WithKeyCondition(expression.KeyEqual(expression.Key("queued"), expression.Value(1))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	var totalQueueSize int
	var exclusiveStartKey map[string]types.AttributeValue
	var peekedRecords int
	allQueueIDs := make([]string, 0)
	processingIDs := make([]string, 0)
	for {
		queryOutput, err := c.dynamoDB.Query(ctx, &dynamodb.QueryInput{
			ProjectionExpression:      expr.Projection(),
			IndexName:                 aws.String(QueueingIndexName),
			TableName:                 aws.String(c.tableName),
			ExpressionAttributeNames:  expr.Names(),
			KeyConditionExpression:    expr.KeyCondition(),
			ScanIndexForward:          aws.Bool(true),
			Limit:                     aws.Int32(250),
			ExpressionAttributeValues: expr.Values(),
			ExclusiveStartKey:         exclusiveStartKey,
		})
		if err != nil {
			return nil, handleDynamoDBError(err)
		}
		exclusiveStartKey = queryOutput.LastEvaluatedKey
		for _, itemMap := range queryOutput.Items {
			totalQueueSize++
			item := Shipment{}
			err := attributevalue.UnmarshalMap(itemMap, &item)
			if err != nil {
				return nil, &UnmarshalingAttributeError{Cause: err}
			}
			if item.SystemInfo.SelectedFromQueue {
				peekedRecords++
				if len(processingIDs) < 100 {
					processingIDs = append(processingIDs, item.ID)
				}
			}
			if len(allQueueIDs) < 100 {
				allQueueIDs = append(allQueueIDs, item.ID)
			}
		}
		if exclusiveStartKey == nil {
			break
		}
	}
	return &QueueStats{
		TotalRecordsInProcessing:   peekedRecords,
		TotalRecordsInQueue:        totalQueueSize,
		TotalRecordsNotStarted:     totalQueueSize - peekedRecords,
		First100IDsInQueue:         allQueueIDs,
		First100SelectedIDsInQueue: processingIDs,
	}, nil
}

// GetDLQStats retrieves statistics about the current state of the Dead Letter Queue (DLQ).
//
// The function queries the DynamoDB table to fetch records that are currently in the DLQ.
// It calculates the total number of records in the DLQ and provides the IDs of the first 100 records.
//
// Parameters:
//
//	ctx: The context for the request, used for timeout and cancellation.
//
// Returns:
//   - A pointer to a DLQStats object containing the calculated statistics.
//   - An error if there's any issue querying the database or processing the results.
//
// Note: The function uses pagination to query the DynamoDB table and will continue querying
// until all records have been fetched or an error occurs.
func (c *queueSDKClient) GetDLQStats(ctx context.Context) (*DLQStats, error) {
	expr, err := expression.NewBuilder().
		WithProjection(expression.NamesList(
			expression.Name("id"),
			expression.Name("DLQ"),
			expression.Name("system_info"))).
		WithKeyCondition(expression.KeyEqual(expression.Key("DLQ"), expression.Value(1))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	var totalDLQSize int
	var lastEvaluatedKey map[string]types.AttributeValue
	listBANs := make([]string, 0)
	for {
		resp, err := c.dynamoDB.Query(ctx, &dynamodb.QueryInput{
			ProjectionExpression:      expr.Projection(),
			IndexName:                 aws.String(DlqQueueingIndexName),
			TableName:                 aws.String(c.tableName),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			KeyConditionExpression:    expr.KeyCondition(),
			Limit:                     aws.Int32(250),
			ScanIndexForward:          aws.Bool(true),
			ExclusiveStartKey:         lastEvaluatedKey,
		})
		if err != nil {
			return nil, handleDynamoDBError(err)
		}
		lastEvaluatedKey = resp.LastEvaluatedKey
		for _, itemMap := range resp.Items {
			totalDLQSize++
			if len(listBANs) < 100 {
				item := Shipment{}
				err := attributevalue.UnmarshalMap(itemMap, &item)
				if err != nil {
					return nil, &UnmarshalingAttributeError{Cause: err}
				}
				listBANs = append(listBANs, item.ID)
			}
		}
		if lastEvaluatedKey == nil {
			break
		}
	}
	return &DLQStats{
		First100IDsInQueue: listBANs,
		TotalRecordsInDLQ:  totalDLQSize,
	}, nil
}

// Get retrieves a shipment record from the database by its ID.
//
// If the provided 'id' is empty, it returns nil and an error indicating that
// the ID is not provided.
//
// Parameters:
//   - ctx (context.Context): The context for the request.
//   - id (string): The unique identifier of the shipment record to retrieve.
//
// Returns:
//   - (*Shipment): A pointer to the retrieved shipment record.
//   - (error): An error if any occurred during the retrieval process, including
//     if the 'id' is empty, the database query fails, or unmarshaling the response
//     fails.
func (c *queueSDKClient) Get(ctx context.Context, id string) (*Shipment, error) {
	if id == "" {
		return nil, &IDNotProvidedError{}
	}
	resp, err := c.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{Value: id},
		},
		TableName:      aws.String(c.tableName),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, handleDynamoDBError(err)
	}
	if resp.Item == nil {
		return nil, nil
	}
	item := Shipment{}
	err = attributevalue.UnmarshalMap(resp.Item, &item)
	if err != nil {
		return nil, &UnmarshalingAttributeError{Cause: err}
	}
	return &item, nil
}

// Put stores a given Shipment object in a DynamoDB table using the PutImpl method.
// The object is stored in the table with its specified ID, creating a new entry if it
// doesn't exist. If an entry with the same ID exists, the method will delete it.
//
// Parameters:
//   - ctx: Context used for timeout, cancellation, and value sharing for the operation.
//   - shipment: The Shipment object to be stored.
//
// Returns:
//   - error: Returns an error if one occurs, otherwise, it returns nil on successful storage.
func (c *queueSDKClient) Put(ctx context.Context, shipment *Shipment) error {
	retrieved, err := c.Get(ctx, shipment.ID)
	if err != nil {
		return err
	}
	if retrieved != nil {
		_, err := c.dynamoDB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(c.tableName),
			Key: map[string]types.AttributeValue{
				"id": &types.AttributeValueMemberS{Value: shipment.ID},
			},
		})
		if err != nil {
			return handleDynamoDBError(err)
		}
	}
	return c.put(ctx, shipment)
}

// Upsert attempts to update an existing Shipment object in a DynamoDB table or inserts it
// if it doesn't exist. It uses the PutImpl method to perform this upsert operation.
// If an entry with the same ID exists, the method will update it.
//
// Parameters:
//   - ctx: Context used for timeout, cancellation, and value sharing for the operation.
//   - shipment: The Shipment object to be upserted.
//
// Returns:
//   - error: Returns an error if one occurs, otherwise, it returns nil on successful upsert.
func (c *queueSDKClient) Upsert(ctx context.Context, shipment *Shipment) error {
	retrieved, err := c.Get(ctx, shipment.ID)
	if err != nil {
		return err
	}
	if retrieved != nil {
		retrieved.Update(shipment, c.clock.Now())
		shipment = retrieved
	}
	return c.put(ctx, shipment)
}

func (c *queueSDKClient) put(ctx context.Context, shipment *Shipment) error {
	item, err := shipment.MarshalMap()
	if err != nil {
		return err
	}
	_, err = c.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      item,
	})
	if err != nil {
		return handleDynamoDBError(err)
	}
	return nil
}

// UpdateStatus changes the status of a record with a given ID to a new status.
// This method is primarily intended for situations where there are operational
// issues or live incidents that need addressing. It is advised not to use this
// call unless explicitly necessary for such circumstances.
//
// The method tries to retrieve the record using the provided ID. If the ID is
// not provided or if the record cannot be found, an appropriate error status is
// returned. If the current status of the record matches the new status, the
// method returns without making any changes. Otherwise, it updates the status
// and the associated metadata.
//
// Parameters:
//   - ctx: The context object, used for timeout, cancellation, and value sharing for the operation.
//   - id: The ID of the record to update.
//   - newStatus: The new status to set for the record.
//
// Returns:
//   - A pointer to a Result object containing the result of the update operation.
//   - An error if one occurs during the process. A nil error indicates successful completion.
func (c *queueSDKClient) UpdateStatus(ctx context.Context, id string, newStatus Status) (*Result, error) {
	if id == "" {
		return nil, &IDNotProvidedError{}
	}
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if shipment == nil {
		return nil, &IDNotFoundError{}
	}
	prevStatus := shipment.SystemInfo.Status
	if prevStatus == newStatus {
		return &Result{
			ID:                   shipment.ID,
			Status:               newStatus,
			LastUpdatedTimestamp: shipment.SystemInfo.LastUpdatedTimestamp,
			Version:              shipment.SystemInfo.Version,
		}, nil
	}
	shipment.ChangeStatus(newStatus, c.clock.Now())
	expr, err := expression.NewBuilder().
		WithUpdate(expression.Add(expression.Name("system_info.version"), expression.Value(1)).
			Set(expression.Name("system_info.status"), expression.Value(shipment.SystemInfo.Status)).
			Set(expression.Name("system_info.last_updated_timestamp"), expression.Value(shipment.SystemInfo.LastUpdatedTimestamp)).
			Set(expression.Name("last_updated_timestamp"), expression.Value(shipment.LastUpdatedTimestamp))).
		WithCondition(expression.Name("system_info.version").Equal(expression.Value(shipment.SystemInfo.Version))).
		Build()
	if err != nil {
		return nil, BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, id, &expr)
	if err != nil {
		return nil, err
	}
	return &Result{
		ID:                   item.SystemInfo.ID,
		Status:               item.SystemInfo.Status,
		LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
		Version:              item.SystemInfo.Version,
	}, nil
}

// Enqueue inserts the provided shipment ID into the queue. If the ID is not provided,
// it returns an error indicating the ID was not provided.
// If the shipment with the given ID cannot be found, it returns an error indicating the ID was not found.
//
// The function performs several checks on the status of the shipment:
// - If the status is UNDER_CONSTRUCTION, it indicates the record is not yet constructed.
// - If the status is not READY_TO_SHIP, it indicates an illegal state.
//
// If all checks pass, the function attempts to update several attributes of the shipment
// in the DynamoDB table. If the update is successful, it retrieves the shipment from
// DynamoDB and assigns it to the result.
//
// Parameters:
//
//	ctx: A context.Context for request. It can be used to cancel the request or to carry deadlines.
//	id: The ID of the shipment to enqueue.
//
// Returns:
//
//	*EnqueueResult: A pointer to the EnqueueResult structure which contains information about the enqueued shipment.
//	error: An error that can occur during the execution, or nil if no errors occurred.
func (c *queueSDKClient) Enqueue(ctx context.Context, id string) (*EnqueueResult, error) {
	if id == "" {
		return nil, &IDNotProvidedError{}
	}
	retrieved, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if retrieved == nil {
		return nil, &IDNotFoundError{}
	}
	preStatus := retrieved.SystemInfo.Status
	if preStatus == StatusUnderConstruction {
		return nil, &RecordNotConstructedError{}
	}
	if preStatus != StatusReadyToShip {
		return nil, &IllegalStateError{}
	}
	retrieved.MarkAsEnqueued(c.clock.Now())
	expr, err := expression.NewBuilder().
		WithUpdate(expression.Add(
			expression.Name("system_info.version"),
			expression.Value(1),
		).Set(
			expression.Name("queued"),
			expression.Value(retrieved.Queued),
		).Set(
			expression.Name("system_info.queued"),
			expression.Value(retrieved.SystemInfo.InQueue),
		).Set(
			expression.Name("system_info.queue_selected"),
			expression.Value(retrieved.SystemInfo.SelectedFromQueue),
		).Set(
			expression.Name("last_updated_timestamp"),
			expression.Value(retrieved.LastUpdatedTimestamp),
		).Set(
			expression.Name("system_info.last_updated_timestamp"),
			expression.Value(retrieved.SystemInfo.LastUpdatedTimestamp),
		).Set(
			expression.Name("system_info.queue_add_timestamp"),
			expression.Value(retrieved.SystemInfo.AddToQueueTimestamp),
		).Set(
			expression.Name("system_info.status"),
			expression.Value(retrieved.SystemInfo.Status),
		)).
		WithCondition(expression.Name("system_info.version").
			Equal(expression.Value(retrieved.SystemInfo.Version))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, id, &expr)
	if err != nil {
		return nil, err
	}
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &EnqueueResult{
		Result: &Result{
			ID:                   item.ID,
			Status:               item.SystemInfo.Status,
			LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
			Version:              item.SystemInfo.Version,
		},
		Shipment: shipment,
	}, nil
}

// Peek peeks at the top of the queue to get the next item without actually removing it.
// It ensures items in the queue that are orphaned or stuck in a processing state for more than
// the allowed visibility timeout are considered for retrieval. It returns the peeked item's details
// encapsulated in a PeekResult structure or an error if something goes wrong.
//
// Parameters:
// - ctx: The context for managing request lifetime and cancelation.
//
// Returns:
//   - *PeekResult: The result of the peek operation, containing details like ID, Version,
//     LastUpdatedTimestamp, Status, and TimestampMillisUTC of the peeked item.
//     It also contains the ReturnValue which denotes the outcome of the operation.
//   - error: An error encountered during the peek operation, if any. Otherwise, nil.
//
// Note:
// The function does not update the top-level attribute `last_updated_timestamp` to
// avoid re-indexing the order.
func (c *queueSDKClient) Peek(ctx context.Context) (*PeekResult, error) {
	expr, err := expression.NewBuilder().
		WithProjection(expression.NamesList(
			expression.Name("id"),
			expression.Name("queued"),
			expression.Name("system_info"))).
		WithKeyCondition(expression.Key("queued").Equal(expression.Value(1))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	var (
		exclusiveStartKey    map[string]types.AttributeValue
		selectedID           string
		selectedVersion      int
		recordForPeekIsFound bool
	)
	for {
		queryResult, err := c.dynamoDB.Query(ctx, &dynamodb.QueryInput{
			ProjectionExpression:      expr.Projection(),
			IndexName:                 aws.String(QueueingIndexName),
			TableName:                 aws.String(c.tableName),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(250),
			ScanIndexForward:          aws.Bool(true),
			ExclusiveStartKey:         exclusiveStartKey,
		})
		if err != nil {
			return nil, handleDynamoDBError(err)
		}
		exclusiveStartKey = queryResult.LastEvaluatedKey
		for _, itemMap := range queryResult.Items {
			item := Shipment{}
			if err = attributevalue.UnmarshalMap(itemMap, &item); err != nil {
				return nil, &UnmarshalingAttributeError{Cause: err}
			}
			visibilityTimeout := time.Duration(c.visibilityTimeoutInMinutes) * time.Minute
			if !item.IsQueueSelected(c.clock.Now(), visibilityTimeout) {
				selectedID = item.ID
				selectedVersion = item.SystemInfo.Version
				recordForPeekIsFound = true
				break
			}
		}
		if recordForPeekIsFound || exclusiveStartKey == nil {
			break
		}
	}
	if selectedID == "" {
		return nil, &EmptyQueueError{}
	}
	shipment, err := c.Get(ctx, selectedID)
	if err != nil {
		return nil, err
	}
	shipment.MarkAsPeeked(c.clock.Now())
	// IMPORTANT
	// please note, we are not updating top-level attribute `last_updated_timestamp` in order to avoid re-indexing the order
	expr, err = expression.NewBuilder().
		WithUpdate(expression.
			Add(expression.Name("system_info.version"), expression.Value(1)).
			Set(expression.Name("system_info.queue_selected"), expression.Value(shipment.SystemInfo.SelectedFromQueue)).
			Set(expression.Name("last_updated_timestamp"), expression.Value(shipment.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.last_updated_timestamp"), expression.Value(shipment.SystemInfo.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.queue_peek_timestamp"), expression.Value(shipment.SystemInfo.PeekFromQueueTimestamp)).
			Set(expression.Name("system_info.peek_utc_timestamp"), expression.Value(shipment.SystemInfo.PeekUTCTimestamp)).
			Set(expression.Name("system_info.status"), expression.Value(shipment.SystemInfo.Status))).
		WithCondition(expression.Name("system_info.version").Equal(expression.Value(selectedVersion))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, shipment.ID, &expr)
	if err != nil {
		return nil, err
	}
	peekedShipment, err := c.Get(ctx, selectedID)
	if err != nil {
		return nil, err
	}
	return &PeekResult{
		Result: &Result{
			ID:                   item.ID,
			Status:               item.SystemInfo.Status,
			LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
			Version:              item.SystemInfo.Version,
		},
		TimestampMillisUTC:   item.SystemInfo.PeekUTCTimestamp,
		PeekedShipmentObject: peekedShipment,
	}, nil
}

// Dequeue attempts to dequeue an item from the Queue. It first peeks at the queue to get an item
// and then attempts to remove that item from the Queue if the peek was successful.
//
// Parameters:
//   - ctx: context.Context is the context for method invocation which can be used for timeout and cancellation.
//
// Returns:
//   - *DequeueResult: the result of the dequeue operation, containing information about the dequeued item.
//   - error: any error encountered during the operation. If successful, this is nil.
func (c *queueSDKClient) Dequeue(ctx context.Context) (*DequeueResult, error) {
	peekResult, err := c.Peek(ctx)
	if err != nil {
		return nil, err
	}
	id := peekResult.ID
	removeResult, err := c.Remove(ctx, id)
	if err != nil {
		return nil, err
	}
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &DequeueResult{
		Result:                 removeResult,
		DequeuedShipmentObject: shipment,
	}, nil
}

// Remove tries to remove an item with a specified ID from the underlying datastore.
// The removal is done by updating attributes of the item in the datastore.
//
// Parameters:
//   - ctx: context.Context is the context for method invocation which can be used for timeout and cancellation.
//   - id: string is the unique identifier of the item to be removed.
//
// Returns:
//   - *Result: the result of the remove operation, containing information about the removed item's status.
//   - error: any error encountered during the operation, especially related to data marshaling and database interactions.
//     If successful and the item is just not found, the error is nil but the Result reflects the status.
func (c *queueSDKClient) Remove(ctx context.Context, id string) (*Result, error) {
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if shipment == nil {
		return nil, &IDNotFoundError{}
	}
	if shipment.IsRemoved() {
		return &Result{
			ID:                   id,
			Status:               shipment.SystemInfo.Status,
			LastUpdatedTimestamp: shipment.SystemInfo.LastUpdatedTimestamp,
			Version:              shipment.SystemInfo.Version,
		}, nil
	}
	ts := clock.FormatRFC3339(c.clock.Now())
	expr, err := expression.NewBuilder().
		WithUpdate(expression.
			Add(expression.Name("system_info.version"), expression.Value(1)).
			Remove(expression.Name("queued")).
			Remove(expression.Name("DLQ")).
			Set(expression.Name("system_info.queued"), expression.Value(0)).
			Set(expression.Name("system_info.queue_selected"), expression.Value(false)).
			Set(expression.Name("system_info.last_updated_timestamp"), expression.Value(ts)).
			Set(expression.Name("last_updated_timestamp"), expression.Value(ts)).
			Set(expression.Name("system_info.queue_remove_timestamp"), expression.Value(ts))).
		WithCondition(expression.Name("system_info.version").Equal(expression.Value(shipment.SystemInfo.Version))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, id, &expr)
	if err != nil {
		return nil, err
	}
	return &Result{
		ID:                   id,
		Status:               item.SystemInfo.Status,
		LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
		Version:              item.SystemInfo.Version,
	}, nil
}

// Restore adds back a record to the queue by its ID. The function updates the
// record in the queue to reflect its restored status.
//
// Parameters:
//
//	ctx: The context to be used for the operation.
//	id: The ID of the record to restore.
//
// Returns:
//
//	*Result: A pointer to a Result object that contains
//	information about the result of the restore operation, such as the version,
//	status, and last updated timestamp.
//
//	error: An error that describes any issues that occurred during the
//	restore operation. If the operation is successful, this will be nil.
func (c *queueSDKClient) Restore(ctx context.Context, id string) (*Result, error) {
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if shipment == nil {
		return nil, &IDNotFoundError{}
	}
	if shipment.IsEnqueued() {
		return &Result{
			ID:                   id,
			Status:               shipment.SystemInfo.Status,
			LastUpdatedTimestamp: shipment.SystemInfo.LastUpdatedTimestamp,
			Version:              shipment.SystemInfo.Version,
		}, nil
	}
	shipment.MarkAsEnqueued(c.clock.Now())
	expr, err := expression.NewBuilder().
		WithUpdate(expression.
			Add(expression.Name("system_info.version"), expression.Value(1)).
			Remove(expression.Name("DLQ")).
			Set(expression.Name("queued"), expression.Value(shipment.Queued)).
			Set(expression.Name("last_updated_timestamp"), expression.Value(shipment.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.queued"), expression.Value(shipment.SystemInfo.InQueue)).
			Set(expression.Name("system_info.queue_selected"), expression.Value(shipment.SystemInfo.SelectedFromQueue)).
			Set(expression.Name("system_info.last_updated_timestamp"), expression.Value(shipment.SystemInfo.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.queue_add_timestamp"), expression.Value(shipment.SystemInfo.AddToQueueTimestamp)).
			Set(expression.Name("system_info.status"), expression.Value(shipment.SystemInfo.Status))).
		WithCondition(expression.Name("system_info.version").Equal(expression.Value(shipment.SystemInfo.Version))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, id, &expr)
	if err != nil {
		return nil, err
	}
	return &Result{
		ID:                   id,
		Status:               item.SystemInfo.Status,
		LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
		Version:              item.SystemInfo.Version,
	}, nil
}

// SendToDLQ sends a specified record with the given ID to the Dead Letter Queue (DLQ).
// The method performs various operations:
// 1. Fetches the shipment details associated with the provided ID.
// 2. Constructs a DynamoDB update expression to mark the record for DLQ and updates timestamps.
// 3. Updates the specified record in the DynamoDB table.
//
// Parameters:
//
//	ctx: The context for the operation. It can be used to control cancelation.
//	id: The ID of the record that needs to be sent to the DLQ.
//
// Returns:
//   - *Result: A pointer to the result structure which contains details like Version, Status, LastUpdatedTimestamp,
//     and ReturnValue indicating the result of the operation (e.g., success, failed due to ID not found, etc.).
//   - error: Non-nil if there was an error during the operation.
func (c *queueSDKClient) SendToDLQ(ctx context.Context, id string) (*Result, error) {
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if shipment == nil {
		return nil, &IDNotFoundError{}
	}
	if shipment.IsDLQ() {
		return &Result{
			ID:                   id,
			Status:               shipment.SystemInfo.Status,
			LastUpdatedTimestamp: shipment.SystemInfo.LastUpdatedTimestamp,
			Version:              shipment.SystemInfo.Version,
		}, nil
	}
	shipment.MarkAsDLQ(c.clock.Now())
	expr, err := expression.NewBuilder().
		WithUpdate(expression.
			Add(expression.Name("system_info.version"), expression.Value(1)).
			Remove(expression.Name("queued")).
			Set(expression.Name("DLQ"), expression.Value(shipment.DLQ)).
			Set(expression.Name("system_info.queued"), expression.Value(shipment.SystemInfo.InQueue)).
			Set(expression.Name("system_info.queue_selected"), expression.Value(shipment.SystemInfo.SelectedFromQueue)).
			Set(expression.Name("last_updated_timestamp"), expression.Value(shipment.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.last_updated_timestamp"), expression.Value(shipment.SystemInfo.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.dlq_add_timestamp"), expression.Value(shipment.SystemInfo.AddToDLQTimestamp)).
			Set(expression.Name("system_info.status"), expression.Value(shipment.SystemInfo.Status))).
		WithCondition(expression.And(
			expression.Name("system_info.version").Equal(expression.Value(shipment.SystemInfo.Version)),
			expression.Name("system_info.queued").Equal(expression.Value(1)),
		)).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, id, &expr)
	if err != nil {
		return nil, err
	}
	return &Result{
		ID:                   id,
		Status:               item.SystemInfo.Status,
		LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
		Version:              item.SystemInfo.Version,
	}, nil
}

// Touch updates the 'last_updated_timestamp' of a given item identified by its 'id'
// and increments its 'version' by 1 in the DynamoDB table.
// It uses optimistic locking based on the item's 'version' to ensure that updates
// are not conflicting with other concurrent updates.
//
// Parameters:
// ctx: The context for this operation. It can be used to time out or cancel the operation.
// id: The identifier of the item to update.
//
// Returns:
// *Result: A result object that contains updated values and status of the operation.
// - If the given 'id' does not exist, the 'ReturnValue' of the result will be set to 'ReturnStatusFailedIDNotFound'.
// - If the operation succeeds in updating the item, the 'ReturnValue' will be set to 'ReturnStatusSUCCESS'.
// - If there is an error while updating in DynamoDB, the 'ReturnValue' will be set to 'ReturnStatusFailedDynamoError'.
// error: An error object indicating any error that occurred during the operation.
// - If there's an error while building the DynamoDB expression, this error is returned.
// - If there's an error unmarshalling the DynamoDB response, this error is returned.
// Otherwise, if the operation succeeds, the error will be 'nil'.
func (c *queueSDKClient) Touch(ctx context.Context, id string) (*Result, error) {
	shipment, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if shipment == nil {
		return nil, &IDNotFoundError{}
	}
	shipment.Touch(c.clock.Now())
	expr, err := expression.NewBuilder().
		WithUpdate(expression.
			Add(expression.Name("system_info.version"), expression.Value(1)).
			Set(expression.Name("last_updated_timestamp"), expression.Value(shipment.LastUpdatedTimestamp)).
			Set(expression.Name("system_info.last_updated_timestamp"), expression.Value(shipment.SystemInfo.LastUpdatedTimestamp))).
		WithCondition(expression.Name("system_info.version").Equal(expression.Value(shipment.SystemInfo.Version))).
		Build()
	if err != nil {
		return nil, &BuildingExpressionError{Cause: err}
	}
	item, err := c.updateDynamoDBItem(ctx, id, &expr)
	if err != nil {
		return nil, err
	}
	return &Result{
		ID:                   id,
		Status:               item.SystemInfo.Status,
		LastUpdatedTimestamp: item.SystemInfo.LastUpdatedTimestamp,
		Version:              item.SystemInfo.Version,
	}, nil
}

// List retrieves a list of Shipments from the DynamoDB table up to the given size.
// The function constructs a DynamoDB scan with specific projection expressions and
// returns the list of found shipments.
//
// Parameters:
//   - ctx: The context to use for the request.
//   - size: The maximum number of items to retrieve.
//
// Returns:
//   - A slice of pointers to Shipment if found.
//   - error if there's any issue in the operation.
func (c *queueSDKClient) List(ctx context.Context, size int32) ([]*Shipment, error) {
	output, err := c.dynamoDB.Scan(ctx, &dynamodb.ScanInput{
		TableName: &c.tableName,
		Limit:     aws.Int32(size),
	})
	if err != nil {
		return nil, handleDynamoDBError(err)
	}
	var shipments []*Shipment
	err = attributevalue.UnmarshalListOfMaps(output.Items, &shipments)
	if err != nil {
		return nil, &UnmarshalingAttributeError{Cause: err}
	}
	return shipments, nil
}

// ListIDs retrieves a list of IDs from the Shipment items in the DynamoDB table
// up to the given size. It uses the List function to retrieve the shipments and
// then extracts the IDs from them.
//
// Parameters:
//   - ctx: The context to use for the request.
//   - size: The maximum number of IDs to retrieve.
//
// Returns:
//   - A slice of string IDs if found.
//   - error if there's any issue in the operation.
func (c *queueSDKClient) ListIDs(ctx context.Context, size int32) ([]string, error) {
	shipments, err := c.List(ctx, size)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(shipments))
	for i, s := range shipments {
		ids[i] = s.ID
	}

	return ids, nil
}

// ListExtendedIDs retrieves a list of extended IDs (formatted as "ID - status: STATUS")
// from the Shipment items in the DynamoDB table up to the given size.
// It uses the List function to retrieve the shipments and then constructs
// the extended ID strings from them.
//
// Parameters:
//   - ctx: The context to use for the request.
//   - size: The maximum number of extended IDs to retrieve.
//
// Returns:
//   - A slice of extended ID strings if found.
//   - error if there's any issue in the operation.
func (c *queueSDKClient) ListExtendedIDs(ctx context.Context, size int32) ([]string, error) {
	shipments, err := c.List(ctx, size)
	if err != nil {
		return nil, err
	}

	extendedIDs := make([]string, len(shipments))
	for i, s := range shipments {
		extendedIDs[i] = fmt.Sprintf("ID: %s, status: %s", s.ID, s.SystemInfo.Status)
	}

	return extendedIDs, nil
}

// Delete removes the shipment record associated with the provided ID from the database.
// It will return an error if the ID is empty or if there's any issue deleting the record.
//
// Parameters:
//   - ctx: The context to be used for the deletion request. It allows for timeout and cancellation.
//   - id: The unique identifier of the shipment record to be deleted.
//
// Returns:
//   - error: Non-nil if there was an error during the delete operation.
func (c *queueSDKClient) Delete(ctx context.Context, id string) error {
	if id == "" {
		return &IDNotProvidedError{}
	}
	_, err := c.dynamoDB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &c.tableName,
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{
				Value: id,
			},
		},
	})
	if err != nil {
		return handleDynamoDBError(err)
	}
	return nil
}

// CreateTestData creates a test data shipment record associated with the provided ID.
// It first ensures that no existing data with the given ID exists by deleting it,
// then creates a shipment record with test data.
// If the ID is empty or there's an issue creating the test data, it will return an error.
//
// Parameters:
//   - ctx: The context to be used for the operation. It allows for timeout and cancellation.
//   - id: The unique identifier for the shipment record to be created.
//
// Returns:
//   - *Shipment: The created shipment record.
//   - error: Non-nil if there was an error during the creation process.
func (c *queueSDKClient) CreateTestData(ctx context.Context, id string) (*Shipment, error) {
	if id == "" {
		return nil, &IDNotProvidedError{}
	}
	err := c.Delete(ctx, id)
	if err != nil {
		return nil, err
	}
	data := newTestShipmentData(id)
	shipment := NewDefaultShipment(id, data, c.clock.Now())
	err = c.Put(ctx, shipment)
	if err != nil {
		return nil, err
	}
	return shipment, nil
}

func newTestShipmentData(id string) *ShipmentData {
	return &ShipmentData{
		ID:    id,
		Data1: "Data 1",
		Data2: "Data 2",
		Data3: "Data 3",
		Items: []ShipmentItem{
			{SKU: "Item-1", Packed: true},
			{SKU: "Item-2", Packed: true},
			{SKU: "Item-3", Packed: true},
		},
	}
}

func (c *queueSDKClient) updateDynamoDBItem(ctx context.Context,
	id string, expr *expression.Expression) (*Shipment, error) {
	outcome, err := c.dynamoDB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{
				Value: id,
			},
		},
		TableName:                 aws.String(c.tableName),
		ConditionExpression:       expr.Condition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		UpdateExpression:          expr.Update(),
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return nil, handleDynamoDBError(err)
	}
	shipment := Shipment{}
	err = attributevalue.UnmarshalMap(outcome.Attributes, &shipment)
	if err != nil {
		return nil, &UnmarshalingAttributeError{Cause: err}
	}
	return &shipment, nil
}

func handleDynamoDBError(err error) error {
	var cause *types.ConditionalCheckFailedException
	if errors.As(err, &cause) {
		return &ConditionalCheckFailedError{Cause: cause}
	}
	return &DynamoDBAPIError{Cause: err}
}
