// Package kube talks to the in-cluster Kubernetes API server directly over the pod's mounted
// ServiceAccount token, deliberately without k8s.io/client-go — this project's go.mod is
// intentionally light (two direct dependencies before this package), and a single ConfigMap PATCH
// needs nothing client-go provides beyond an HTTP client, the in-cluster CA bundle, and a bearer
// token, all available from the two standard in-cluster mount paths below.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// Client patches ConfigMap data keys via the in-cluster Kubernetes API.
type Client struct {
	httpClient *http.Client
	apiServer  string // e.g. "https://10.43.0.1:443"
	tokenPath  string
}

// NewInClusterClient builds a Client from the pod's mounted ServiceAccount token, CA bundle, and
// the kubelet-injected KUBERNETES_SERVICE_HOST/PORT env vars. Returns an error if any of those
// aren't present — i.e. if not actually running in a pod.
func NewInClusterClient() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("kube: KUBERNETES_SERVICE_HOST/PORT not set — not running in-cluster")
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("kube: read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("kube: no valid certs found in %s", caPath)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		return nil, fmt.Errorf("kube: service account token not mounted: %w", err)
	}
	return &Client{
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
		apiServer:  fmt.Sprintf("https://%s:%s", host, port),
		tokenPath:  tokenPath,
	}, nil
}

// PatchConfigMapKey does a JSON merge-patch of a single data key on a ConfigMap — equivalent to
// `kubectl patch configmap NAME --type merge -p '{"data":{"KEY":"..."}}'`. The token is re-read
// from disk on every call rather than cached, since kubelet rotates the projected token
// periodically and an in-memory copy would eventually be rejected as expired.
func (c *Client) PatchConfigMapKey(ctx context.Context, namespace, name, key string, value []byte) error {
	token, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return fmt.Errorf("kube: read service account token: %w", err)
	}

	patch, err := json.Marshal(map[string]any{"data": map[string]string{key: string(value)}})
	if err != nil {
		return fmt.Errorf("kube: marshal patch body: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/configmaps/%s", c.apiServer, namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(patch))
	if err != nil {
		return fmt.Errorf("kube: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+string(token))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kube: PATCH %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("kube: PATCH %s: unexpected status %d: %s", url, resp.StatusCode, body)
	}
	return nil
}
