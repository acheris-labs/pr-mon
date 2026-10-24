// Package aws is the small part of AWS the Telegram handler talks to: Lambda invoke and SQS,
// signed with Signature Version 4 using only the standard library.
package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Credentials are an IAM access key.
type Credentials struct {
	AccessKeyID, SecretAccessKey string
}

// Headers a proxy or the transport may change, so they are never signed.
var unsignedHeaders = map[string]bool{"user-agent": true, "authorization": true}

// Sign adds SigV4 `X-Amz-Date` and `Authorization` headers to a request whose
// body is `body`. It signs `host`, `x-amz-date` and every other header already
// on the request except `User-Agent` and `Authorization`.
func Sign(request *http.Request, body []byte, credentials Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	request.Header.Set("X-Amz-Date", amzDate)

	headers := map[string]string{"host": host(request)}
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if unsignedHeaders[lower] {
			continue
		}
		trimmed := make([]string, len(values))
		for i, value := range values {
			trimmed[i] = strings.Join(strings.Fields(value), " ")
		}
		headers[lower] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		fmt.Fprintf(&canonicalHeaders, "%s:%s\n", name, headers[name])
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		request.Method,
		canonicalURI(request),
		canonicalQuery(request),
		canonicalHeaders.String(),
		signedHeaders,
		hexSHA256(body),
	}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(canonical))

	key := hmacSHA256([]byte("AWS4"+credentials.SecretAccessKey), date)
	for _, part := range []string{region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	request.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		credentials.AccessKeyID, scope, signedHeaders, signature))
}

func host(request *http.Request) string {
	if request.Host != "" {
		return request.Host
	}
	return request.URL.Host
}

// canonicalURI encodes the path as sent once more, as every service but S3
// expects.
func canonicalURI(request *http.Request) string {
	path := request.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	return uriEncode(path, false)
}

func canonicalQuery(request *http.Request) string {
	query := request.URL.Query()
	pairs := make([]string, 0, len(query))
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, uriEncode(name, true)+"="+uriEncode(value, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes everything but RFC 3986's unreserved characters,
// and `/` too unless `encodeSlash`.
func uriEncode(s string, encodeSlash bool) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/' && !encodeSlash:
			out.WriteByte(c)
		default:
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
