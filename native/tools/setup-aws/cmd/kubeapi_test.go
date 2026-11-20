package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fixedPresigner presigns to a fixed URL.
type fixedPresigner string

func (p fixedPresigner) PresignGetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: string(p), Method: http.MethodGet}, nil
}

const presignedURL = "https://sts.us-west-2.amazonaws.com/?Action=GetCallerIdentity&X-Amz-Signature=abc"

// kubeTestCluster is a cluster whose API is srv, trusting srv's certificate.
func kubeTestCluster(srv *httptest.Server) *ekstypes.Cluster {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return &ekstypes.Cluster{Name: aws.String("demo"), Endpoint: aws.String(srv.URL),
		CertificateAuthority: &ekstypes.Certificate{Data: aws.String(base64.StdEncoding.EncodeToString(ca))}}
}

// The client lists the nodes of a label selector with an EKS bearer token,
// over TLS that trusts the cluster's CA, and reads each node's Ready
// condition.
func TestKubeClientListsNodes(t *testing.T) {
	var gotAuth, gotSelector, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotSelector = r.Header.Get("Authorization"), r.URL.Path, r.URL.Query().Get("labelSelector")
		w.Write([]byte(`{"kind":"NodeList","items":[
			{"metadata":{"name":"ip-1","labels":{"ate.dev/pool":"gvisor"}},"status":{"allocatable":{"cpu":"8"},"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}]}},
			{"metadata":{"name":"ip-2","labels":{"ate.dev/pool":"gvisor"}},"status":{"conditions":[{"type":"Ready","status":"False"}]}},
			{"metadata":{"name":"ip-3"},"status":{}}]}`))
	}))
	defer srv.Close()
	k, err := newKubeClient(kubeTestCluster(srv), fixedPresigner(presignedURL))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := k.Nodes(context.Background(), "ate.dev/pool=gvisor")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Bearer k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presignedURL)); gotAuth != want {
		t.Errorf("Authorization %q, want %q", gotAuth, want)
	}
	if gotPath != "/api/v1/nodes" || gotSelector != "ate.dev/pool=gvisor" {
		t.Errorf("GET %s?labelSelector=%s", gotPath, gotSelector)
	}
	if len(nodes) != 3 || !nodes[0].Ready || nodes[1].Ready || nodes[2].Ready || nodes[0].Name != "ip-1" ||
		nodes[0].Labels["ate.dev/pool"] != "gvisor" || nodes[0].Allocatable["cpu"] != "8" {
		t.Errorf("nodes %+v", nodes)
	}
}

// A refused request is an error with the status, never with the token; a
// server with another CA is not trusted; a cluster without a CA is refused.
func TestKubeClientRefusals(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	k, err := newKubeClient(kubeTestCluster(srv), fixedPresigner(presignedURL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = k.Nodes(context.Background(), "ate.dev/pool=system")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
	if strings.Contains(err.Error(), "k8s-aws-v1") || strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Errorf("the error carries the token: %v", err)
	}

	other := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a server with another certificate was trusted")
	}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake
	other.StartTLS()
	defer other.Close()
	c := kubeTestCluster(srv)
	c.Endpoint = aws.String(other.URL)
	k, _ = newKubeClient(c, fixedPresigner(presignedURL))
	_, err = k.Nodes(context.Background(), "a=b")
	if err == nil || strings.Contains(err.Error(), "k8s-aws-v1") {
		t.Errorf("another CA: %v", err)
	}
	// It fails the same way every time: the Ready wait must stop on it.
	if kubeTransient(err) {
		t.Errorf("a certificate that fails verification is taken as transient: %v", err)
	}

	c.CertificateAuthority = nil
	if _, err := newKubeClient(c, fixedPresigner(presignedURL)); err == nil {
		t.Error("a cluster without a CA was accepted")
	}
}

// A failed list says whether it may work again: no answer (a closed
// server, a client timeout), 429 and a 5xx may; 401, 403 and another 4xx
// would come again.
func TestKubeClientTransientErrors(t *testing.T) {
	for status, transient := range map[int]bool{
		http.StatusTooManyRequests: true, http.StatusInternalServerError: true, http.StatusBadGateway: true,
		http.StatusServiceUnavailable: true, http.StatusGatewayTimeout: true,
		http.StatusUnauthorized: false, http.StatusForbidden: false, http.StatusNotFound: false, http.StatusBadRequest: false,
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", status)
		}))
		k, err := newKubeClient(kubeTestCluster(srv), fixedPresigner(presignedURL))
		if err != nil {
			t.Fatal(err)
		}
		_, err = k.Nodes(context.Background(), "a=b")
		if err == nil || kubeTransient(err) != transient || !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Errorf("%d: %v, transient %v, want %v", status, err, kubeTransient(err), transient)
		}
		srv.Close()
	}

	// No answer: the server is gone, or answers after the client's timeout.
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	k, err := newKubeClient(kubeTestCluster(gone), fixedPresigner(presignedURL))
	if err != nil {
		t.Fatal(err)
	}
	gone.Close()
	if _, err := k.Nodes(context.Background(), "a=b"); err == nil || !kubeTransient(err) {
		t.Errorf("a closed server: %v, transient %v", err, kubeTransient(err))
	}
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	k, err = newKubeClient(kubeTestCluster(slow), fixedPresigner(presignedURL))
	if err != nil {
		t.Fatal(err)
	}
	k.http.Timeout = 20 * time.Millisecond
	if _, err := k.Nodes(context.Background(), "a=b"); err == nil || !kubeTransient(err) {
		t.Errorf("a timeout: %v, transient %v", err, kubeTransient(err))
	}
	// Not a request that failed on the way.
	if kubeTransient(nil) || kubeTransient(apiErr("ThrottlingException")) {
		t.Error("nil or an AWS error is a transient Kubernetes API error")
	}
}

// selfSigned is a certificate for 127.0.0.1 that no other test server has.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The token is `aws eks get-token`'s: a presigned GetCallerIdentity URL for
// 60 seconds that signs the cluster name in x-k8s-aws-id. Presigning runs
// locally, with made-up credentials.
func TestEKSTokenSignsTheClusterName(t *testing.T) {
	c := sts.New(sts.Options{Region: "us-west-2", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, nil
	})})
	tok, err := eksToken(context.Background(), sts.NewPresignClient(c), "demo")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, "k8s-aws-v1."))
	if !strings.HasPrefix(tok, "k8s-aws-v1.") || err != nil {
		t.Fatalf("token %q (%v)", tok, err)
	}
	u, err := url.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "sts.us-west-2.amazonaws.com" || q.Get("Action") != "GetCallerIdentity" || q.Get("X-Amz-Expires") != "60" ||
		!strings.Contains(q.Get("X-Amz-SignedHeaders"), "x-k8s-aws-id") || q.Get("X-Amz-Signature") == "" {
		t.Errorf("presigned URL %s", u)
	}
}
