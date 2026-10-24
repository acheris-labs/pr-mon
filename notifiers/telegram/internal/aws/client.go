package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Longer than SQS's 20-second long poll, so a quiet poll isn't cut short.
var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

// Client calls Lambda and SQS in one region. Empty endpoints mean AWS's own;
// tests point them at a local server.
type Client struct {
	Credentials                 Credentials
	Region                      string
	HTTP                        *http.Client
	LambdaEndpoint, SQSEndpoint string
	Now                         func() time.Time
}

// FunctionError is an error the invoked Lambda function raised, as opposed to
// one invoking it.
type FunctionError struct {
	Message string
}

func (e *FunctionError) Error() string { return e.Message }

// HTTPError is a non-2xx reply from AWS, keeping the status and body so
// callers can tell one failure from another.
type HTTPError struct {
	Status string
	Body   string
}

func (e *HTTPError) Error() string { return e.Status + ": " + e.Body }

// ReceiptGone reports whether err says a receipt handle is no longer valid
// (expired or already used), so the message can't be deleted and need not be.
func ReceiptGone(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	return strings.Contains(httpErr.Body, "ReceiptHandleIsInvalid") ||
		(strings.Contains(httpErr.Body, "InvalidParameterValue") && strings.Contains(httpErr.Body, "ReceiptHandle"))
}

// SQSMessage is one received queue message.
type SQSMessage struct {
	ReceiptHandle, Body string
}

func (c *Client) lambdaEndpoint() string {
	if c.LambdaEndpoint != "" {
		return c.LambdaEndpoint
	}
	return "https://lambda." + c.Region + ".amazonaws.com"
}

func (c *Client) sqsEndpoint() string {
	if c.SQSEndpoint != "" {
		return c.SQSEndpoint
	}
	return "https://sqs." + c.Region + ".amazonaws.com"
}

// Invoke runs a Lambda function synchronously and returns what it returned.
func (c *Client) Invoke(ctx context.Context, function string, payload any) (json.RawMessage, error) {
	url := c.lambdaEndpoint() + "/2015-03-31/functions/" + function + "/invocations"
	response, body, err := c.post(ctx, url, "lambda", payload, nil)
	if err != nil {
		return nil, fmt.Errorf("invoke %s: %w", function, err)
	}
	if response.Header.Get("X-Amz-Function-Error") != "" {
		var failure struct {
			ErrorMessage string `json:"errorMessage"`
		}
		if json.Unmarshal(body, &failure) != nil || failure.ErrorMessage == "" {
			return nil, &FunctionError{Message: string(body)}
		}
		return nil, &FunctionError{Message: failure.ErrorMessage}
	}
	return json.RawMessage(body), nil
}

// Receive long-polls a queue for up to `wait`, returning at most `max` messages.
func (c *Client) Receive(ctx context.Context, queueURL string, wait time.Duration, max int) ([]SQSMessage, error) {
	request := map[string]any{
		"QueueUrl":            queueURL,
		"WaitTimeSeconds":     int(wait / time.Second),
		"MaxNumberOfMessages": max,
	}
	body, err := c.sqs(ctx, "ReceiveMessage", request)
	if err != nil {
		return nil, err
	}
	var reply struct{ Messages []SQSMessage }
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("sqs ReceiveMessage: %w", err)
	}
	return reply.Messages, nil
}

// DeleteMessage removes a received message from its queue.
func (c *Client) DeleteMessage(ctx context.Context, queueURL, receipt string) error {
	_, err := c.sqs(ctx, "DeleteMessage", map[string]any{"QueueUrl": queueURL, "ReceiptHandle": receipt})
	return err
}

// sqs calls one action of SQS's JSON protocol.
func (c *Client) sqs(ctx context.Context, action string, request any) ([]byte, error) {
	headers := map[string]string{
		"Content-Type": "application/x-amz-json-1.0",
		"X-Amz-Target": "AmazonSQS." + action,
	}
	_, body, err := c.post(ctx, c.sqsEndpoint()+"/", "sqs", request, headers)
	if err != nil {
		return nil, fmt.Errorf("sqs %s: %w", action, err)
	}
	return body, nil
}

// post sends `payload` as JSON, signed for `service`, and returns the response
// and its body; a non-2xx status is an error carrying the body.
func (c *Client) post(ctx context.Context, url, service string, payload any, headers map[string]string) (*http.Response, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	Sign(request, data, c.Credentials, c.Region, service, now())

	client := c.HTTP
	if client == nil {
		client = defaultHTTP
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode/100 != 2 {
		return nil, nil, &HTTPError{Status: response.Status, Body: string(bytes.TrimSpace(body))}
	}
	return response, body, nil
}
