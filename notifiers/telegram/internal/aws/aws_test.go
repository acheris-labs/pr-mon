package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	testCredentials = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	testNow         = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
)

// AWS's published get-vanilla signing example.
func TestSignGetVanilla(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	Sign(request, nil, testCredentials, "us-east-1", "service", testNow)

	if got := request.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := request.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, want)
	}
}

// The signature botocore computes for the same request.
func TestSignSQSPost(t *testing.T) {
	body := []byte(`{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/123456789012/q.fifo","WaitTimeSeconds":20}`)
	request, err := http.NewRequest(http.MethodPost, "https://sqs.us-east-1.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-amz-json-1.0")
	request.Header.Set("X-Amz-Target", "AmazonSQS.ReceiveMessage")
	request.Header.Set("User-Agent", "not signed")
	Sign(request, body, testCredentials, "us-east-1", "sqs", testNow)

	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/sqs/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date;x-amz-target, " +
		"Signature=82fe94d3b04c19ba3416d775c25ab6e17e12b6ea2702632e5728e948e26c851c"
	if got := request.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, want)
	}
}

func testClient(server *httptest.Server) *Client {
	return &Client{
		Credentials:    testCredentials,
		Region:         "us-east-1",
		LambdaEndpoint: server.URL,
		SQSEndpoint:    server.URL,
		Now:            func() time.Time { return testNow },
	}
}

func TestInvokeReturnsPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/2015-03-31/functions/telegram-webhook-sender/invocations" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/lambda/aws4_request") {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"message":"hi"}` {
			t.Errorf("body = %s", body)
		}
		w.Write([]byte(`{"ok":true,"message_id":7}`))
	}))
	defer server.Close()

	got, err := testClient(server).Invoke(context.Background(), "telegram-webhook-sender", map[string]string{"message": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true,"message_id":7}` {
		t.Errorf("payload = %s", got)
	}
}

func TestInvokeFunctionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Function-Error", "Unhandled")
		w.Write([]byte(`{"errorMessage":"boom"}`))
	}))
	defer server.Close()

	_, err := testClient(server).Invoke(context.Background(), "f", map[string]string{})
	var functionError *FunctionError
	if !errors.As(err, &functionError) || functionError.Message != "boom" {
		t.Fatalf("err = %#v, want *FunctionError{boom}", err)
	}
}

func TestInvokeHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"denied"}`, http.StatusForbidden)
	}))
	defer server.Close()

	_, err := testClient(server).Invoke(context.Background(), "f", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want a 403 error", err)
	}
}

func TestReceiveAndDelete(t *testing.T) {
	const queue = "https://sqs.us-east-1.amazonaws.com/123456789012/q.fifo"
	reply := `{"Messages":[{"ReceiptHandle":"r1","Body":"b"}]}`
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/sqs/aws4_request") {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		switch target := r.Header.Get("X-Amz-Target"); target {
		case "AmazonSQS.ReceiveMessage":
			want := map[string]any{"QueueUrl": queue, "WaitTimeSeconds": 20.0, "MaxNumberOfMessages": 10.0}
			if !reflect.DeepEqual(request, want) {
				t.Errorf("receive request = %v, want %v", request, want)
			}
			w.Write([]byte(reply))
		case "AmazonSQS.DeleteMessage":
			if request["QueueUrl"] != queue {
				t.Errorf("delete queue = %v", request["QueueUrl"])
			}
			deleted = append(deleted, request["ReceiptHandle"].(string))
			w.Write([]byte(`{}`))
		default:
			t.Errorf("X-Amz-Target = %q", target)
		}
	}))
	defer server.Close()
	client := testClient(server)
	ctx := context.Background()

	messages, err := client.Receive(ctx, queue, 20*time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []SQSMessage{{ReceiptHandle: "r1", Body: "b"}}; !reflect.DeepEqual(messages, want) {
		t.Errorf("messages = %v, want %v", messages, want)
	}
	if err := client.DeleteMessage(ctx, queue, "r1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []string{"r1"}) {
		t.Errorf("deleted = %v", deleted)
	}

	reply = `{}`
	messages, err = client.Receive(ctx, queue, 20*time.Second, 10)
	if err != nil || len(messages) != 0 {
		t.Errorf("empty receive = %v, %v; want no messages", messages, err)
	}
}

func TestSQSErrorIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"The specified queue does not exist."}`))
	}))
	defer server.Close()

	_, err := testClient(server).Receive(context.Background(), "q", 20*time.Second, 10)
	if err == nil || !strings.Contains(err.Error(), "The specified queue does not exist.") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultEndpoints(t *testing.T) {
	client := &Client{Region: "eu-west-2"}
	if got := client.lambdaEndpoint(); got != "https://lambda.eu-west-2.amazonaws.com" {
		t.Errorf("lambda endpoint = %q", got)
	}
	if got := client.sqsEndpoint(); got != "https://sqs.eu-west-2.amazonaws.com" {
		t.Errorf("sqs endpoint = %q", got)
	}
}

func TestHTTPErrorKeepsStatusAndBody(t *testing.T) {
	const reply = `{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"The specified queue does not exist."}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(reply))
	}))
	defer server.Close()

	_, err := testClient(server).Receive(context.Background(), "q", 20*time.Second, 10)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want an *HTTPError", err)
	}
	if httpErr.Status != "400 Bad Request" || httpErr.Body != reply {
		t.Errorf("HTTPError = %+v", httpErr)
	}
	if !strings.Contains(err.Error(), "The specified queue does not exist.") {
		t.Errorf("err = %v, want the body in the text", err)
	}
}

func TestReceiptGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#ReceiptHandleIsInvalid","message":"The input receipt handle \"x\" is not a valid receipt handle."}`}, true},
		{"expired fifo", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#InvalidParameterValue","message":"Value x for parameter ReceiptHandle is invalid. Reason: The receipt handle has expired."}`}, true},
		{"other bad parameter", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#InvalidParameterValue","message":"Value 0 for parameter WaitTimeSeconds is invalid."}`}, false},
		{"queue missing", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"The specified queue does not exist."}`}, false},
		{"wrapped", fmt.Errorf("sqs DeleteMessage: %w", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#ReceiptHandleIsInvalid"}`}), true},
		{"network", errors.New("dial tcp: no route to host"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReceiptGone(c.err); got != c.want {
				t.Errorf("ReceiptGone = %v, want %v", got, c.want)
			}
		})
	}
}
