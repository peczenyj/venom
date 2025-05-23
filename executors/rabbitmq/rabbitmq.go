package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mitchellh/mapstructure"

	"github.com/ovh/venom"

	"github.com/streadway/amqp"
)

// Name of executor
const Name = "rabbitmq"

// New returns a new Executor
func New() venom.Executor {
	return &Executor{}
}

// Message represents the object sended or received from rabbitmq
type Message struct {
	Value           string     `json:"value" yaml:"value"`
	Headers         amqp.Table `json:"headers" yaml:"headers"`
	Persistent      bool       `json:"persistent" yaml:"persistent"`
	ContentType     string     `json:"content_type" yaml:"contentType"`
	ContentEncoding string     `json:"content_encoding" yaml:"contentEncoding"`
	ReplyTo         string     `json:"reply_to" yaml:"replyTo"`
}

// Executor represents a Test Exec
type Executor struct {
	Addrs string `json:"addrs" yaml:"addrs"`
	// WithTLS  bool     `json:"with_tls" yaml:"withTLS"`
	User     string `json:"user" yaml:"user"`
	Password string `json:"password" yaml:"password"`

	// ClientType must be "consumer" or "producer"
	ClientType string `json:"client_type" yaml:"clientType"`

	// QName represents the RabbitMQ queue name
	QName string `json:"q_name" yaml:"qName"`
	// Durable represents the RabbitMQ durable parameter
	Durable bool `json:"durable" yaml:"durable"`

	// Exchange represents the RabbitMQ exchange
	Exchange string `json:"exchange" yaml:"exchange"`
	// RoutingKey represents the RabbitMQ routing key
	ExchangeType string `json:"exchange_type" yaml:"exchangeType"`
	// ExchangeType represents the type of exchange (fanout, etc..)
	RoutingKey string `json:"routing_key" yaml:"routingKey"`

	// Represents the limit of message will be read. After limit, consumer stop read message
	MessageLimit int `json:"message_limit" yaml:"messageLimit"`

	// Used when ClientType is producer
	// Messages represents the message sended by producer
	Messages []Message `json:"messages" yaml:"messages"`
}

// Result represents a step result.
type Result struct {
	TimeSeconds float64       `json:"timeseconds" yaml:"timeSeconds"`
	Body        []string      `json:"body" yaml:"body"`
	Messages    []interface{} `json:"messages" yaml:"messages"`
	BodyJSON    []interface{} `json:"bodyjson" yaml:"bodyJSON"`
	Headers     []amqp.Table  `json:"headers" yaml:"headers"`
	Err         string        `json:"err" yaml:"error"`
}

// ZeroValueResult return an empty implementation of this executor result
func (Executor) ZeroValueResult() interface{} {
	return Result{}
}

// GetDefaultAssertions return default assertions for type exec
func (Executor) GetDefaultAssertions() *venom.StepAssertions {
	return &venom.StepAssertions{Assertions: []venom.Assertion{"result.error ShouldBeEmpty"}}
}

// Run execute TestStep of type exec
func (Executor) Run(ctx context.Context, step venom.TestStep) (interface{}, error) {
	var e Executor
	if err := mapstructure.Decode(step, &e); err != nil {
		return nil, err
	}

	start := time.Now()
	result := Result{}

	// Default values
	if e.Addrs == "" {
		return nil, errors.New("address is mandatory")
	}
	if e.ExchangeType == "" {
		e.ExchangeType = amqp.ExchangeFanout
	}
	if e.MessageLimit == 0 {
		e.MessageLimit = 1
	}

	switch e.ClientType {
	case "publisher":
		workdir := venom.StringVarFromCtx(ctx, "venom.testsuite.workdir")
		err := e.publishMessages(ctx, workdir, nil, nil, false)
		if err != nil {
			result.Err = err.Error()
			return nil, err
		}
	case "subscriber":
		var err error
		result.Body, result.BodyJSON, result.Messages, result.Headers, err = e.consumeMessages(ctx)
		if err != nil {
			result.Err = err.Error()
			return nil, err
		}
	case "client":
		conn, ch, err := e.openChannel(ctx)
		if err != nil {
			result.Err = err.Error()
			return nil, err
		}
		defer ch.Close()
		defer conn.Close()
		delivery, consumererr := ch.Consume("amq.rabbitmq.reply-to", "", true, false, false, false, nil)
		if consumererr != nil {
			return nil, consumererr
		}
		venom.Info(ctx, "Reply consumer started.")

		workdir := venom.StringVarFromCtx(ctx, "venom.testsuite.workdir")
		err = e.publishMessages(ctx, workdir, conn, ch, true)
		if err != nil {
			result.Err = err.Error()
			return nil, err
		}

		d := <-delivery
		body := []string{}
		bodyJSON := []interface{}{}
		body, bodyJSON = e.processMessage(ctx, d, true, body, bodyJSON)
		result.Body = body
		result.BodyJSON = bodyJSON
	default:
		return nil, fmt.Errorf("clientType %q must be publisher or subscriber or client", e.ClientType)
	}

	elapsed := time.Since(start)
	result.TimeSeconds = elapsed.Seconds()

	return result, nil
}

func (e Executor) publishMessages(ctx context.Context, workdir string, connection *amqp.Connection, channel *amqp.Channel, rpc bool) error {
	var ch *amqp.Channel
	var err error
	if connection == nil || channel == nil {
		var conn *amqp.Connection
		conn, ch, err = e.openChannel(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer ch.Close()
	} else {
		ch = channel
	}

	// If an exchange if defined
	routingKey := e.RoutingKey
	if e.Exchange != "" {
		if err := ch.ExchangeDeclare(
			e.Exchange,     // name
			e.ExchangeType, // type
			e.Durable,      // durable
			false,          // auto-deleted
			false,          // internal
			false,          // no-wait
			nil,            // arguments
		); err != nil {
			return err
		}
		venom.Debug(ctx, "exchange declared %q %q", e.Exchange, e.ExchangeType)
	} else {
		if e.QName == "" {
			return errors.New("QName is mandatory")
		}
		routingKey = e.QName
		_, err := ch.QueueDeclare(
			e.QName,   // name
			e.Durable, // durable
			false,     // delete when unused
			false,     // exclusive
			false,     // no-wait
			nil,       // arguments
		)
		if err != nil {
			return err
		}
		venom.Debug(ctx, "Queue declared '%s'", e.QName)
	}

	venom.Debug(ctx, "%d message to send", len(e.Messages))
	for i := range e.Messages {
		deliveryMode := amqp.Persistent
		if !e.Messages[i].Persistent {
			deliveryMode = amqp.Transient
		}
		var replyTo string = e.Messages[i].ReplyTo
		if rpc {
			replyTo = "amq.rabbitmq.reply-to"
		}
		err = ch.Publish(
			e.Exchange, // exchange
			routingKey, // routing key
			false,      // mandatory
			false,      // immediate
			amqp.Publishing{
				DeliveryMode:    deliveryMode,
				ContentType:     e.Messages[i].ContentType,
				ContentEncoding: e.Messages[i].ContentEncoding,
				ReplyTo:         replyTo,
				Body:            []byte(e.Messages[i].Value),
				Headers:         e.Messages[i].Headers,
			})
		if err != nil {
			return err
		}
		venom.Debug(ctx, "Message %q sent (exchange: %q, routing key: %q)", e.Messages[i].Value, e.Exchange, routingKey)
	}

	return nil
}

func (e Executor) openChannel(ctx context.Context) (*amqp.Connection, *amqp.Channel, error) {
	uri, err := amqp.ParseURI(e.Addrs)
	if err != nil {
		return nil, nil, err
	}
	uri.Username = e.User
	uri.Password = e.Password

	conn, err := amqp.Dial(uri.String())
	if err != nil {
		return nil, nil, err
	}
	venom.Debug(ctx, "connection opened")

	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, err
	}
	venom.Debug(ctx, "channel opened")
	return conn, ch, nil
}

func (e Executor) processMessage(ctx context.Context, msg amqp.Delivery, ok bool, body []string, bodyJSON []interface{}) ([]string, []interface{}) {
	venom.Debug(ctx, "message: %t %s %s %s", ok, msg.RoutingKey, msg.MessageId, msg.ContentType)
	venom.Debug(ctx, "receive: %s", string(msg.Body))
	body = append(body, string(msg.Body))

	bodyJSONArray := []interface{}{}
	if err := venom.JSONUnmarshal(msg.Body, &bodyJSONArray); err != nil {
		bodyJSONMap := map[string]interface{}{}
		venom.JSONUnmarshal(msg.Body, &bodyJSONMap) //nolint
		bodyJSON = append(bodyJSON, bodyJSONMap)
	} else {
		bodyJSON = append(bodyJSON, bodyJSONArray)
	}

	return body, bodyJSON
}

func (e Executor) consumeMessages(ctx context.Context) ([]string, []interface{}, []interface{}, []amqp.Table, error) {
	conn, ch, err := e.openChannel(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer conn.Close()
	defer ch.Close()

	q, err := ch.QueueDeclare(
		e.QName,   // name
		e.Durable, // durable
		false,     // delete when unused
		false,     // exclusive
		false,     // no-wait
		nil,       // arguments
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	venom.Debug(ctx, "Q declared '%s'", q.Name)

	// If an exchange if defined
	if e.Exchange != "" {
		err = ch.ExchangeDeclare(
			e.Exchange,     // name
			e.ExchangeType, // type
			e.Durable,      // durable
			false,          // auto-deleted
			false,          // internal
			false,          // no-wait
			nil,            // arguments
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		venom.Debug(ctx, "exchange declared '%s' '%s'", e.Exchange, e.ExchangeType)

		err = ch.QueueBind(
			q.Name,       // queue name
			e.RoutingKey, // routing key
			e.Exchange,   // exchange
			false,        // no-wait
			nil,          // arguments
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		venom.Debug(ctx, "Q binded '%s' '%s'", q.Name, e.RoutingKey)
	}

	body := []string{}
	bodyJSON := []interface{}{}
	messages := []interface{}{}
	headers := []amqp.Table{}

	for i := 0; i < e.MessageLimit; i++ {
		venom.Debug(ctx, "Read message n° %d", i)

		msg, ok, err := ch.Get(q.Name, true) // Read one message from RabbitMQ
		if err != nil {
			return nil, nil, nil, nil, err
		}

		headers = append(headers, msg.Headers)
		messages = append(messages, msg)
		body, bodyJSON = e.processMessage(ctx, msg, ok, body, bodyJSON)
	}

	return body, bodyJSON, messages, headers, err
}
