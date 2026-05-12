package common

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	c "github.com/VersusControl/versus-incident/pkg/config"
)

// sqsClient is the subset of *sqs.Client the listener uses. Carved out
// so unit tests can inject a fake without spinning up a real AWS SDK
// client + endpoint resolver dance.
type sqsClient interface {
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// SQSListener long-polls an AWS SQS queue and dispatches each message
// body to the supplied handler. A message is deleted only after the
// handler returns nil — handler errors leave the message visible again
// after the visibility timeout, so the queue's redrive policy retries
// or sends to DLQ as configured by the operator.
type SQSListener struct {
	client                   sqsClient
	queueURL                 string
	maxNumberOfMessages      int32
	waitTimeSeconds          int32
	visibilityTimeoutSeconds int32

	// receiveErrorBackoff is how long to wait after a ReceiveMessage
	// error before retrying. Keeps a misconfigured queue from
	// hot-looping. Exposed as a field for tests.
	receiveErrorBackoff time.Duration
}

// NewSQSListener constructs a listener using the standard AWS SDK
// credential chain (env vars → shared credentials → IMDS / IAM role).
// cfg.Region is honored when set; otherwise AWS_REGION / shared config
// resolves it.
func NewSQSListener(cfg c.SQSConfig) (*SQSListener, error) {
	if cfg.QueueURL == "" {
		return nil, fmt.Errorf("sqs: queue_url is required")
	}

	awsOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		awsOpts = append(awsOpts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsOpts...)
	if err != nil {
		return nil, fmt.Errorf("sqs: load aws config: %w", err)
	}

	maxN := int32(cfg.MaxNumberOfMessages)
	if maxN <= 0 || maxN > 10 {
		maxN = 10
	}
	wait := int32(cfg.WaitTimeSeconds)
	if wait < 0 || wait > 20 {
		wait = 20
	}
	visibility := int32(cfg.VisibilityTimeoutSeconds)
	if visibility <= 0 {
		visibility = 30
	}

	return &SQSListener{
		client:                   sqs.NewFromConfig(awsCfg),
		queueURL:                 cfg.QueueURL,
		maxNumberOfMessages:      maxN,
		waitTimeSeconds:          wait,
		visibilityTimeoutSeconds: visibility,
		receiveErrorBackoff:      5 * time.Second,
	}, nil
}

// StartListening loops forever, long-polling the queue and calling
// handler for each received message body. The function returns only
// on an unrecoverable error (currently none — ReceiveMessage errors
// trigger a backoff and retry). The caller runs this in its own
// goroutine and relies on process shutdown to stop it.
//
// Handler errors do NOT cause the listener to exit — they just skip
// the DeleteMessage so the message becomes visible again after
// VisibilityTimeout. This is the SQS-native retry path.
func (s *SQSListener) StartListening(handler func(content *map[string]interface{}) error) error {
	for {
		out, err := s.client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(s.queueURL),
			MaxNumberOfMessages: s.maxNumberOfMessages,
			WaitTimeSeconds:     s.waitTimeSeconds,
			VisibilityTimeout:   s.visibilityTimeoutSeconds,
		})
		if err != nil {
			log.Printf("sqs: receive message failed: %v", err)
			time.Sleep(s.receiveErrorBackoff)
			continue
		}
		for _, msg := range out.Messages {
			s.processMessage(msg, handler)
		}
	}
}

// processMessage decodes one SQS message body as JSON and dispatches
// it to handler. On success the message is deleted; on any error
// (decode or handler) it is left for retry. Returns nothing — errors
// are logged for the operator and the loop continues.
func (s *SQSListener) processMessage(msg types.Message, handler func(content *map[string]interface{}) error) {
	if msg.Body == nil {
		log.Printf("sqs: message %s has nil body, deleting", aws.ToString(msg.MessageId))
		s.deleteMessage(msg)
		return
	}
	var content map[string]interface{}
	if err := json.Unmarshal([]byte(*msg.Body), &content); err != nil {
		// Non-JSON bodies are likely operator misconfiguration (e.g. a
		// raw string was published). Delete so it doesn't infinitely
		// re-arrive; operator should fix the publisher.
		log.Printf("sqs: message %s body is not JSON: %v; deleting", aws.ToString(msg.MessageId), err)
		s.deleteMessage(msg)
		return
	}
	if err := handler(&content); err != nil {
		log.Printf("sqs: handler failed for message %s: %v (left for redrive)", aws.ToString(msg.MessageId), err)
		return
	}
	s.deleteMessage(msg)
}

// deleteMessage acks the message. Failures are logged but not
// surfaced — at-least-once delivery means a re-arrival is recoverable;
// blocking the loop on a transient DeleteMessage error is worse.
func (s *SQSListener) deleteMessage(msg types.Message) {
	_, err := s.client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(s.queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	})
	if err != nil {
		log.Printf("sqs: delete message %s failed: %v", aws.ToString(msg.MessageId), err)
	}
}
