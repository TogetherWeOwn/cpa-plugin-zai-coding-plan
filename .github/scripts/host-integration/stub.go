package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	anthropicUpstreamPath = "/api/anthropic/v1/messages"
	openAIUpstreamPath    = "/api/coding/paas/v4/chat/completions"
	quotaUpstreamPath     = "/api/monitor/usage/quota/limit"
)

type upstreamStub struct {
	server *http.Server
	mu     sync.Mutex
	counts map[string]int
	paths  []string
}

func startUpstreamStub(root string) (*upstreamStub, string, error) {
	certPath, keyPath, caPath, err := writeTestCertificate(root)
	if err != nil {
		return nil, "", err
	}
	stub := &upstreamStub{
		server: &http.Server{Addr: "127.0.0.1:443", ReadHeaderTimeout: 3 * time.Second},
		counts: make(map[string]int),
	}
	stub.server.Handler = http.HandlerFunc(stub.serveHTTP)
	listener, err := net.Listen("tcp", stub.server.Addr)
	if err != nil {
		return nil, "", fmt.Errorf("listen for Z.ai upstream stub: %w", err)
	}
	go func() {
		_ = stub.server.ServeTLS(listener, certPath, keyPath)
	}()
	return stub, caPath, nil
}

func (stub *upstreamStub) serveHTTP(response http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	stub.mu.Lock()
	stub.counts[path]++
	stub.paths = append(stub.paths, request.Method+" "+request.URL.RequestURI())
	stub.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	switch path {
	case anthropicUpstreamPath:
		if request.Method != http.MethodPost || request.Header.Get("X-CPA-Smoke-Lane") != "anthropic" {
			http.Error(response, `{"error":"invalid anthropic smoke request"}`, http.StatusBadRequest)
			return
		}
		_, _ = response.Write([]byte(`{"id":"msg_smoke","type":"message","role":"assistant","model":"smoke-upstream-anthropic","content":[{"type":"text","text":"anthropic smoke ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`))
	case openAIUpstreamPath:
		if request.Method != http.MethodPost || request.Header.Get("X-CPA-Smoke-Lane") != "openai" {
			http.Error(response, `{"error":"invalid OpenAI smoke request"}`, http.StatusBadRequest)
			return
		}
		_, _ = response.Write([]byte(`{"id":"chatcmpl-smoke","object":"chat.completion","created":0,"model":"smoke-upstream-openai","choices":[{"index":0,"message":{"role":"assistant","content":"openai smoke ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	case quotaUpstreamPath:
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+smokePlanKey {
			http.Error(response, `{"error":"invalid quota smoke request"}`, http.StatusUnauthorized)
			return
		}
		resetFiveHour := time.Now().Add(5 * time.Hour).UnixMilli()
		resetWeekly := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
		_ = json.NewEncoder(response).Encode(map[string]any{
			"code": 200, "success": true,
			"data": map[string]any{"level": "pro", "limits": []map[string]any{
				{"type": "CREDIT_LIMIT", "unit": 3, "number": 5, "usage": 12000, "currentValue": 1, "remaining": 11999, "nextResetTime": resetFiveHour},
				{"type": "CREDIT_LIMIT", "unit": 6, "number": 1, "usage": 60000, "currentValue": 2, "remaining": 59998, "nextResetTime": resetWeekly},
			}},
		})
	default:
		http.Error(response, `{"error":"unexpected upstream path"}`, http.StatusNotFound)
	}
}

func (stub *upstreamStub) assertRequests() error {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, path := range []string{anthropicUpstreamPath, openAIUpstreamPath} {
		if stub.counts[path] != 1 {
			return fmt.Errorf("upstream request count for %s = %d, want 1; observed %s", path, stub.counts[path], strings.Join(stub.paths, ", "))
		}
	}
	return nil
}

func writeTestCertificate(root string) (string, string, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", "", err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "api.z.ai"},
		DNSNames:              []string{"api.z.ai"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", "", err
	}
	certPath := filepath.Join(root, "api-z-ai.pem")
	keyPath := filepath.Join(root, "api-z-ai-key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", "", err
	}
	return certPath, keyPath, certPath, nil
}
