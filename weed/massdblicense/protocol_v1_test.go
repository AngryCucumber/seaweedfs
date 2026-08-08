package massdblicense

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func TestEmbeddedP1GoldenArtifacts(t *testing.T) {
	rootBytes := decodeGolden(t, goldenRootPublic)
	block, rest := pem.Decode(rootBytes)
	if block == nil || len(rest) != 0 {
		t.Fatal("invalid embedded root public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	root := parsed.(*ecdsa.PublicKey)
	roots := map[string]*ecdsa.PublicKey{"massdb-test-root-1": root}
	keyset, err := VerifyKeyset(decodeGolden(t, goldenKeyset), roots, 1767225600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if keyset.SHA256 != "f3f32036e3e6746b2249fee11d62c19242b89169386153e5972cfbddc8fbe158" {
		t.Fatalf("keyset sha=%s", keyset.SHA256)
	}

	valid, err := VerifyLicense(decodeGolden(t, goldenValidLicense), keyset, 1767225600, 31536000, nil)
	if err != nil || valid.SHA256 != "b3f71e8f014c7eaf0f81db83a13803b34648617d3baf2e38b3e44ef0700e1745" {
		t.Fatalf("valid license: %+v %v", valid, err)
	}
	if _, err := VerifyLicense(decodeGolden(t, goldenExpiredLicense), keyset, 1767225600, 31536000, nil); Code(err) != "MASSDB_LICENSE_EXPIRED" {
		t.Fatalf("expired code=%s err=%v", Code(err), err)
	}
	if _, err := VerifyLicense(decodeGolden(t, goldenTamperedLicense), keyset, 1767225600, 31536000, nil); Code(err) != "MASSDB_LICENSE_SIGNATURE_INVALID" {
		t.Fatalf("tampered code=%s err=%v", Code(err), err)
	}

	challenge, _ := ParseHex32("1111111111111111111111111111111111111111111111111111111111111111")
	active, _ := ParseHex32("2222222222222222222222222222222222222222222222222222222222222222")
	recovered, err := VerifyClockRecovery(decodeGolden(t, goldenClockRecovery), keyset, ClockContext{
		Challenge:                challenge,
		DeploymentUUID:           "00000000-0000-4000-8000-000000000099",
		ActiveLicenseSHA256:      active,
		ObservedMaxSeenWallClock: 1768000000,
		MaxAcceptedSequence:      6,
		CurrentWallClock:         1767225600,
	})
	if err != nil || recovered.SHA256 != "8141149315e3ad158c52e55a85fb84f41d4ca3afb72ebb3f92664aaaade17a72" {
		t.Fatalf("clock recovery: %+v %v", recovered, err)
	}
	bundled, err := VerifyRecoveryBundle(decodeGolden(t, goldenRecoveryBundle), roots, 1767225600, 31536000, 0)
	if err != nil || bundled.SHA256 != valid.SHA256 {
		t.Fatalf("bundle: %+v %v", bundled, err)
	}
}

func decodeGolden(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

const goldenRootPublic = "LS0tLS1CRUdJTiBQVUJMSUMgS0VZLS0tLS0KTUZrd0V3WUhLb1pJemowQ0FRWUlLb1pJemowREFRY0RRZ0FFYTBMQmN5UVhwU2VRVGJCMk1oMUR3aDQ0SXNNRQo3MWxuSnBQMUxoVXd3TlpuMVk3ODE4ekU3d2pwYTZ6UEkvLzVvTENBRnIvT05lQmV5cWJmaHRMZHZnPT0KLS0tLS1FTkQgUFVCTElDIEtFWS0tLS0tCg=="
const goldenKeyset = "0oRXogEmBFJtYXNzZGItdGVzdC1yb290LTGgWQEJpwEBAnVNQVNTREJfVFJVU1RFRF9LRVlTRVQDZk1BU1NEQgQBBRppVbaoBoKkAXNtYXNzZGItdGVzdC1jbG9jay0xAm5DTE9DS19SRUNPVkVSWQNYIP8fNoE5UX2LxsOKmaXfc78PUJ5SAO2axxURZLFDzk9FBFggYCL9Y2T5q51le17Lagjc5r1xopjmLJmJDYvQ1mOxIRakAXVtYXNzZGItdGVzdC1saWNlbnNlLTECb0xJQ0VOU0VfU0lHTklORwNYIO4voGpbS4XrUulf3+MCgXo7SGij+SYd8gqS++AKwuezBFggM9NCgdu8ZD7UNVDxCKS2R5JAqu+hXXq/9WSe1KCNWG4HgFhAabq+wn7bS/h+bIpRSAFa7OpxnheLnkF8cy9d07CUa0ixPfvd2e6ejSnfZpDwsUpBtuPTh22/2zVhH2Uvh+WJ1A=="
const goldenValidLicense = "0oRYGqIBJgRVbWFzc2RiLXRlc3QtbGljZW5zZS0xoFg+pQEBAngkMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAxA2ZNQVNTREIEGmlVtqgFGml9RgBYQDXjfKtQyUqrAIbjJfqOxsASlFAYcwzqcCRdEjiyYXkHGNw8Pn3xsDiC8WKTw56yHYYDqtQNYYxMPjqhRlvdpSc="
const goldenExpiredLicense = "0oRYGqIBJgRVbWFzc2RiLXRlc3QtbGljZW5zZS0xoFg+pQEBAngkMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAyA2ZNQVNTREIEGmlVtkQFGmlVuQBYQHYHARbBkRzHaHyePsUknIP1+xOtG5RovVTXzIjkRynpsroiQn71bBIMbMebnco1P/n5yrS8G9Oi1UBTlkkZZ7I="
const goldenTamperedLicense = "0oRYGqIBJgRVbWFzc2RiLXRlc3QtbGljZW5zZS0xoFg+pQEBAngkMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAxA2ZNQVNTREIEGmlVtqgFGml9RgBYQDXjfKtQyUqrAIbjJfqOxsASlFAYcwzqcCRdEjiyYXkHGNw8Pn3xsDiC8WKTw56yHYYDqtQNYYxMPjqhRlvdpSY="
const goldenClockRecovery = "0oRYGKIBJgRTbWFzc2RiLXRlc3QtY2xvY2stMaBYqasBAQJ1TUFTU0RCX0NMT0NLX1JFQ09WRVJZA2ZNQVNTREIEBwVYIBERERERERERERERERERERERERERERERERERERERERERBngkMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDk5B1ggIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIIGmlhigAJGmlVuQAKGmlVtqgLGmlZqyhYQL37jOfWZpiRKieZoGItwAf8y4D2QlNibvTAOkyaT0POseVYTk9OOUCLzauJYjpEb1nTnUFzfeFt6P77G3jqIJ4="
const goldenRecoveryBundle = "pAEBAnglTUFTU0RCX0tFWVNFVF9MSUNFTlNFX1JFQ09WRVJZX0JVTkRMRQNZAWnShFeiASYEUm1hc3NkYi10ZXN0LXJvb3QtMaBZAQmnAQECdU1BU1NEQl9UUlVTVEVEX0tFWVNFVANmTUFTU0RCBAEFGmlVtqgGgqQBc21hc3NkYi10ZXN0LWNsb2NrLTECbkNMT0NLX1JFQ09WRVJZA1gg/x82gTlRfYvGw4qZpd9zvw9QnlIA7ZrHFRFksUPOT0UEWCBgIv1jZPmrnWV7XstqCNzmvXGimOYsmYkNi9DWY7EhFqQBdW1hc3NkYi10ZXN0LWxpY2Vuc2UtMQJvTElDRU5TRV9TSUdOSU5HA1gg7i+galtLhetS6V/f4wKBejtIaKP5Jh3yCpL74ArC57MEWCAz00KB27xkPtQ1UPEIpLZHkkCq76Fder/1ZJ7UoI1YbgeAWEBpur7CfttL+H5silFIAVrs6nGeF4ueQXxzL13TsJRrSLE9+93Z7p6NKd9mkPCxSkG249OHbb/bNWEfZS+H5YnUBFih0oRYGqIBJgRVbWFzc2RiLXRlc3QtbGljZW5zZS0xoFg+pQEBAngkMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAxA2ZNQVNTREIEGmlVtqgFGml9RgBYQDXjfKtQyUqrAIbjJfqOxsASlFAYcwzqcCRdEjiyYXkHGNw8Pn3xsDiC8WKTw56yHYYDqtQNYYxMPjqhRlvdpSc="
