// Package sqs is the AWS SQS adapter. Locally it talks to LocalStack through
// the same SDK the production code uses, so the FIFO semantics this platform
// depends on — MessageGroupId ordering and MessageDeduplicationId collapsing
// duplicate sends — are exercised for real rather than simulated.
package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/integration"
)

// Queue names provisioned by this platform.
//
// The names are given without the ".fifo" suffix: SQS appends it, and a name
// that already carries a dot is rejected. Both directions are FIFO so the
// ordering guarantees the design relies on — one group per aggregate, and
// deduplication by event id — are actually enforced by the broker.
const (
	WagerQueueName  = "wager-transactions"
	WagerDLQName    = "wager-transactions-dlq"
	EventsQueueName = "wager-events"
	EventsDLQName   = "wager-events-dlq"

	// FifoSuffix is the suffix SQS appends to a FIFO queue's name.
	FifoSuffix = ".fifo"

	defaultVisibilitySecs = 30
)

// FifoName returns the broker-visible name of a FIFO queue.
func FifoName(name string) string { return name + FifoSuffix }

// Client wraps the SQS API with the handful of operations this platform needs.
type Client struct {
	api    *sqs.Client
	region string
}

// New builds a client for the configured endpoint.
func New(ctx context.Context, cfg config.SQS) (*Client, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("sqs: load aws config: %w", err)
	}

	api := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
		o.RetryMaxAttempts = 5
	})

	return &Client{api: api, region: cfg.Region}, nil
}

// Inbound is one received message.
type Inbound struct {
	MessageID     string
	ReceiptHandle string
	Body          string
	ReceiveCount  int
	Attributes    map[string]string
}

// SendRaw publishes an already-encoded body.
//
// It is what Send is built on, and what the local seeding tool uses to put a
// provider-shaped message on the queue.
func (c *Client) SendRaw(ctx context.Context, queueURL, body, groupID, dedupID string) error {
	_, err := c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               &queueURL,
		MessageBody:            &body,
		MessageGroupId:         &groupID,
		MessageDeduplicationId: &dedupID,
	})
	if err != nil {
		return fmt.Errorf("sqs: send to %s: %w", queueURL, err)
	}
	return nil
}

// Send publishes a message to a queue.
//
// groupID keeps every event of one aggregate in order, and dedupID is the
// event's own identity: a publisher that crashes after the broker accepted a
// send but before it recorded the fact republishes with the same id, and the
// FIFO queue drops it as the duplicate it is.
func (c *Client) Send(ctx context.Context, queueURL string, message integration.Message, groupID, dedupID string) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("sqs: encode message: %w", err)
	}
	return c.SendRaw(ctx, queueURL, string(body), groupID, dedupID)
}

// Receive polls a queue for up to max messages, waiting at most wait.
func (c *Client) Receive(ctx context.Context, queueURL string, max int32, wait time.Duration) ([]Inbound, error) {
	out, err := c.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            &queueURL,
		MaxNumberOfMessages: max,
		WaitTimeSeconds:     int32(wait.Seconds()),
		VisibilityTimeout:   defaultVisibilitySecs,
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{
			sqstypes.MessageSystemAttributeNameSentTimestamp,
		},
		MessageAttributeNames: []string{"All"},
	})
	if err != nil {
		return nil, fmt.Errorf("sqs: receive from %s: %w", queueURL, err)
	}

	inbound := make([]Inbound, 0, len(out.Messages))
	for _, message := range out.Messages {
		item := Inbound{Body: awsStringValue(message.Body)}
		if message.MessageId != nil {
			item.MessageID = *message.MessageId
		}
		if message.ReceiptHandle != nil {
			item.ReceiptHandle = *message.ReceiptHandle
		}
		if message.Attributes != nil {
			item.Attributes = map[string]string(message.Attributes)
			item.ReceiveCount = parseInt(message.Attributes[string(sqstypes.MessageSystemAttributeNameApproximateReceiveCount)])
		}
		if message.MessageAttributes != nil {
			for key, attribute := range message.MessageAttributes {
				if attribute.StringValue != nil {
					item.Attributes["attr:"+key] = *attribute.StringValue
				}
			}
		}
		inbound = append(inbound, item)
	}
	return inbound, nil
}

// Delete removes a message from the queue. It is called only after the
// transaction that handled it has committed.
func (c *Client) Delete(ctx context.Context, queueURL, receiptHandle string) error {
	_, err := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      &queueURL,
		ReceiptHandle: &receiptHandle,
	})
	if err != nil {
		return fmt.Errorf("sqs: delete from %s: %w", queueURL, err)
	}
	return nil
}

// Release hands a message back before its visibility expires, so another
// instance can take it over immediately instead of waiting out the timeout.
func (c *Client) Release(ctx context.Context, queueURL, receiptHandle string, delay time.Duration) error {
	_, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          &queueURL,
		ReceiptHandle:     &receiptHandle,
		VisibilityTimeout: int32(delay.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("sqs: release on %s: %w", queueURL, err)
	}
	return nil
}

// Ready probes the broker, backing the readiness check.
func (c *Client) Ready(ctx context.Context) error {
	_, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: awsString(FifoName(WagerQueueName))})
	if err != nil {
		return fmt.Errorf("sqs: readiness: %w", err)
	}
	return nil
}

// QueueURL resolves a queue name to its URL.
func (c *Client) QueueURL(ctx context.Context, name string) (string, error) {
	out, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: awsString(FifoName(name))})
	if err != nil {
		return "", fmt.Errorf("sqs: resolve queue %s: %w", name, err)
	}
	return awsStringValue(out.QueueUrl), nil
}

// Provisioning describes the queues this platform needs.
type Provisioning struct {
	Main       string
	DeadLetter string
	Redrive    int
	Visibility time.Duration
}

// Provision creates the queues and their redrive policy, idempotently.
//
// Both directions are provisioned: operations come in through the wager queue,
// integration events go out through the events queue, and each has a dead-letter
// queue so a message that exhausts its retries is retained instead of looping.
func (c *Client) Provision(ctx context.Context, p Provisioning) error {
	if p.Redrive < 1 {
		p.Redrive = 5
	}
	if p.Visibility <= 0 {
		p.Visibility = 30 * time.Second
	}

	dlqURL, err := c.createQueue(ctx, p.DeadLetter, 0)
	if err != nil {
		return err
	}
	dlqARN, err := c.queueARN(ctx, dlqURL)
	if err != nil {
		return err
	}

	if _, err := c.createQueue(ctx, p.Main, p.Visibility); err != nil {
		return err
	}
	mainURL, err := c.QueueURL(ctx, p.Main)
	if err != nil {
		return err
	}
	_, err = c.api.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &mainURL,
		Attributes: map[string]string{
			"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":%q}`, dlqARN, fmt.Sprint(p.Redrive)),
		},
	})
	if err != nil {
		return fmt.Errorf("sqs: set redrive policy on %s: %w", p.Main, err)
	}
	return nil
}

func (c *Client) createQueue(ctx context.Context, name string, visibility time.Duration) (string, error) {
	// FIFO is requested through the attribute: the SDK models it as an attribute
	// rather than a field, and the ".fifo" suffix on the name is what the broker
	// keys off.
	attributes := map[string]string{
		"FifoQueue":                 "true",
		"ContentBasedDeduplication": "false",
	}
	if visibility > 0 {
		attributes["VisibilityTimeout"] = fmt.Sprint(int(visibility.Seconds()))
	}
	_, err := c.api.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  awsString(FifoName(name)),
		Attributes: attributes,
	})
	if err != nil {
		var exists *sqstypes.QueueNameExists
		if errors.As(err, &exists) || strings.Contains(err.Error(), "QueueAlreadyExists") {
			return c.QueueURL(ctx, name)
		}
		return "", fmt.Errorf("sqs: create queue %s: %w", name, err)
	}
	return c.QueueURL(ctx, name)
}

func (c *Client) queueARN(ctx context.Context, queueURL string) (string, error) {
	out, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return "", fmt.Errorf("sqs: read queue arn: %w", err)
	}
	arn := out.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	if arn == "" {
		return "", errors.New("sqs: queue has no arn")
	}
	return arn, nil
}

func parseInt(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return n
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func awsString(v string) *string { return &v }
func awsBool(v bool) *bool       { return &v }
func awsInt32(v int32) *int32    { return &v }

func awsStringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// Runtime is a provisioned client together with the URLs of the queues this
// platform owns.
//
// Provisioning happens once, at start-up, before any worker runs. It is
// idempotent, so restarting an instance never destroys a queue or the messages
// in it.
type Runtime struct {
	Client *Client

	WagerQueue            string
	WagerDeadLetterQueue  string
	EventsQueue           string
	EventsDeadLetterQueue string
}

// Provision creates the client, creates the queues with their redrive policies,
// and resolves their URLs.
//
// The order matters: a consumer must not start against a queue that does not
// exist yet, and a publisher must not publish into a URL nobody has resolved.
func Provision(ctx context.Context, cfg config.SQS) (*Runtime, error) {
	client, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}

	visibility := cfg.VisibilityTimeout
	if visibility <= 0 {
		visibility = defaultVisibilitySecs * time.Second
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 5
	}

	runtime := &Runtime{Client: client}
	provision := func(main, deadLetter string) (string, string, error) {
		if err := client.Provision(ctx, Provisioning{
			Main:       main,
			DeadLetter: deadLetter,
			Redrive:    maxAttempts,
			Visibility: visibility,
		}); err != nil {
			return "", "", err
		}
		mainURL, err := client.QueueURL(ctx, main)
		if err != nil {
			return "", "", err
		}
		dlqURL, err := client.QueueURL(ctx, deadLetter)
		if err != nil {
			return "", "", err
		}
		return mainURL, dlqURL, nil
	}

	if runtime.WagerQueue, runtime.WagerDeadLetterQueue, err = provision(WagerQueueName, WagerDLQName); err != nil {
		return nil, err
	}
	if runtime.EventsQueue, runtime.EventsDeadLetterQueue, err = provision(EventsQueueName, EventsDLQName); err != nil {
		return nil, err
	}

	// Explicitly configured URLs win, so an operator can point a worker at a
	// queue this process did not create.
	if cfg.QueueURL != "" {
		runtime.WagerQueue = cfg.QueueURL
	}
	if cfg.DLQURL != "" {
		runtime.WagerDeadLetterQueue = cfg.DLQURL
	}
	if cfg.EventsQueueURL != "" {
		runtime.EventsQueue = cfg.EventsQueueURL
	}
	return runtime, nil
}

// QueueAttributes returns the attributes of a queue, used to assert that the
// provisioned queues really are FIFO and really have a redrive policy.
func (c *Client) QueueAttributes(ctx context.Context, queueURL string) (map[string]string, error) {
	out, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameFifoQueue,
			sqstypes.QueueAttributeNameRedrivePolicy,
			sqstypes.QueueAttributeNameVisibilityTimeout,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqs: read attributes of %s: %w", queueURL, err)
	}
	attributes := make(map[string]string, len(out.Attributes))
	for key, value := range out.Attributes {
		attributes[string(key)] = value
	}
	return attributes, nil
}
