package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// signAWSv4 adds AWS Signature V4 authentication headers to req.
// body must be the exact bytes that will be sent (nil/empty for bodyless requests).
// If accessKey is empty the function is a no-op so callers need no nil-guard.
func signAWSv4(req *http.Request, accessKey, secretKey, region string, body []byte) {
	if accessKey == "" || secretKey == "" {
		return
	}
	if body == nil {
		body = []byte{}
	}
	if region == "" {
		region = "us-east-1"
	}

	t := time.Now().UTC()
	date := t.Format("20060102")
	datetime := t.Format("20060102T150405Z")
	payloadHash := awsSHA256Hex(body)
	host := req.URL.Host

	req.Host = host
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Signed headers must be listed in sorted order.
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + datetime + "\n"

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	// url.Values.Encode() sorts by key, which satisfies the AWS requirement.
	canonicalQS := req.URL.Query().Encode()

	canonicalReq := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQS,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credScope := fmt.Sprintf("%s/%s/s3/aws4_request", date, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		datetime,
		credScope,
		awsSHA256Hex([]byte(canonicalReq)),
	}, "\n")

	sigKey := awsDeriveKey(secretKey, date, region, "s3")
	sig := hex.EncodeToString(awsHMAC(sigKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		accessKey, credScope, signedHeaders, sig,
	))
}

func awsDeriveKey(secret, date, region, service string) []byte {
	return awsHMAC(
		awsHMAC(
			awsHMAC(
				awsHMAC([]byte("AWS4"+secret), []byte(date)),
				[]byte(region),
			),
			[]byte(service),
		),
		[]byte("aws4_request"),
	)
}

func awsHMAC(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func awsSHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
