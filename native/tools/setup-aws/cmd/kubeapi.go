package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// setup-aws reads one thing from the Kubernetes API: whether the nodes of
// a self-managed node group are Ready. EKS reports nothing about nodes it
// does not manage, so the wait asks the API itself, as kubectl would, with
// a token from the caller's AWS credentials. The caller already has
// cluster-admin from the access phase.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/cluster-auth.html

// kubeNode is what the wait reads of a node.
type kubeNode struct {
	Name        string
	Labels      map[string]string
	Ready       bool
	Allocatable map[string]string
}

// nodeLister lists the nodes that match a label selector.
type nodeLister interface {
	Nodes(ctx context.Context, labelSelector string) ([]kubeNode, error)
}

// stsPresigner presigns sts:GetCallerIdentity (sts.PresignClient).
type stsPresigner interface {
	PresignGetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// kubeTimeout bounds one request to the Kubernetes API.
var kubeTimeout = 15 * time.Second

type kubeClient struct {
	cluster, endpoint string
	http              *http.Client
	sts               stsPresigner
}

// newKubeClient is a client of cluster c's API that trusts only the
// cluster's own certificate authority.
func newKubeClient(c *ekstypes.Cluster, p stsPresigner) (*kubeClient, error) {
	ca := ""
	if c.CertificateAuthority != nil {
		ca = aws.ToString(c.CertificateAuthority.Data)
	}
	pem, err := base64.StdEncoding.DecodeString(ca)
	pool := x509.NewCertPool()
	endpoint := aws.ToString(c.Endpoint)
	if endpoint == "" || err != nil || !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("cluster %s reports no endpoint or certificate authority", aws.ToString(c.Name))
	}
	// Go's client, like kubectl, honours HTTPS_PROXY.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &kubeClient{cluster: aws.ToString(c.Name), endpoint: strings.TrimSuffix(endpoint, "/"),
		http: &http.Client{Transport: t, Timeout: kubeTimeout}, sts: p}, nil
}

// eksToken is a bearer token for cluster, as `aws eks get-token` makes it:
// a presigned sts:GetCallerIdentity URL that names the cluster in a signed
// header. EKS takes it for about 15 minutes, so each request makes a new one.
func eksToken(ctx context.Context, p stsPresigner, cluster string) (string, error) {
	req, err := p.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(so *sts.Options) {
			so.APIOptions = append(so.APIOptions,
				smithyhttp.SetHeaderValue("x-k8s-aws-id", cluster),
				smithyhttp.SetHeaderValue("X-Amz-Expires", "60"))
		})
	})
	if err != nil {
		return "", fmt.Errorf("presign sts:GetCallerIdentity for the Kubernetes API: %w", err)
	}
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}

// nodeList is the part of a v1 NodeList the wait reads.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Allocatable map[string]string `json:"allocatable"`
			Conditions  []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// kubeError is a request to the Kubernetes API that failed on the way:
// Status is the HTTP status of the answer, 0 when none came whole (a
// transport error or a timeout), or -1 when the server's certificate failed
// verification.
type kubeError struct {
	Status int
	err    error
}

func (e *kubeError) Error() string { return e.err.Error() }
func (e *kubeError) Unwrap() error { return e.err }

// certificateError reports a TLS failure to verify the server's certificate.
func certificateError(err error) bool {
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci)
}

// kubeTransient reports whether err is a failed request that may work when
// made again: no whole answer, 429 (too many requests) or a 5xx. Any other
// answer (401, 403, another 4xx) would come again.
func kubeTransient(err error) bool {
	var ke *kubeError
	return errors.As(err, &ke) && (ke.Status == 0 || ke.Status == http.StatusTooManyRequests || ke.Status >= 500)
}

// Nodes lists the nodes that match labelSelector. Any answer but 200 is a
// kubeError that names the status; the token is never part of one.
func (k *kubeClient) Nodes(ctx context.Context, labelSelector string) ([]kubeNode, error) {
	token, err := eksToken(ctx, k.sts, k.cluster)
	if err != nil {
		return nil, err
	}
	u := k.endpoint + "/api/v1/nodes?" + url.Values{"labelSelector": {labelSelector}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	failed := func(status int, err error) error {
		return &kubeError{Status: status, err: fmt.Errorf("list the nodes of cluster %s: %w", k.cluster, err)}
	}
	resp, err := k.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		// A certificate the cluster CA does not sign (a wrong CA, or a
		// TLS-inspecting proxy) fails the same way on every try.
		if certificateError(err) {
			return nil, failed(-1, err)
		}
		return nil, failed(0, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, failed(0, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, failed(resp.StatusCode, errors.New("the Kubernetes API answered "+resp.Status))
	}
	var list nodeList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("list the nodes of cluster %s: %w", k.cluster, err)
	}
	out := make([]kubeNode, 0, len(list.Items))
	for _, it := range list.Items {
		n := kubeNode{Name: it.Metadata.Name, Labels: it.Metadata.Labels, Allocatable: it.Status.Allocatable}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		out = append(out, n)
	}
	return out, nil
}
