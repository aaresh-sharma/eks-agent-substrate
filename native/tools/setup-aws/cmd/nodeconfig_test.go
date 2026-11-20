package cmd

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

var testJoin = nodeJoin{Cluster: "sub", Endpoint: "https://ABC.gr7.us-west-2.eks.amazonaws.com",
	CA: "Q0EtREFUQQ==", ServiceCIDR: "10.100.0.0/16"}

// parseUserData decodes user data the way cloud-init and nodeadm read it:
// base64, a MIME multi-part document, and the NodeConfig part.
func parseUserData(t *testing.T, ud string) (contentTypes []string, nc map[string]any) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatalf("user data is not base64: %v", err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("user data is not a MIME document: %v\n%s", err, raw)
	}
	if v := msg.Header.Get("MIME-Version"); v != "1.0" {
		t.Fatalf("MIME-Version = %q", v)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("Content-Type = %q (%v)", msg.Header.Get("Content-Type"), err)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("user data is not a complete MIME multi-part document: %v", err)
		}
		ct := p.Header.Get("Content-Type")
		contentTypes = append(contentTypes, ct)
		if ct == "application/node.eks.aws" {
			if err := json.NewDecoder(p).Decode(&nc); err != nil {
				t.Fatalf("NodeConfig part is not JSON: %v", err)
			}
		}
	}
	return contentTypes, nc
}

func TestNodeUserDataJoinsWithLabelsAndTaints(t *testing.T) {
	labels := map[string]string{"ate.dev/pool": "gvisor", "a.example/x": "1"}
	taints := []ekstypes.Taint{{Key: aws.String("ate.dev/sandboxClass"), Value: aws.String("gvisor"), Effect: ekstypes.TaintEffectNoSchedule}}
	ud, err := nodeUserData(testJoin, labels, taints)
	if err != nil {
		t.Fatal(err)
	}
	cts, nc := parseUserData(t, ud)
	if !reflect.DeepEqual(cts, []string{"application/node.eks.aws"}) {
		t.Fatalf("parts = %v, want one NodeConfig part", cts)
	}
	want := map[string]any{
		"apiVersion": "node.eks.aws/v1alpha1", "kind": "NodeConfig",
		"spec": map[string]any{
			"cluster": map[string]any{"name": "sub", "apiServerEndpoint": testJoin.Endpoint,
				"certificateAuthority": testJoin.CA, "cidr": "10.100.0.0/16"},
			"kubelet": map[string]any{
				"flags":  []any{"--node-labels=a.example/x=1,ate.dev/pool=gvisor"},
				"config": map[string]any{"registerWithTaints": []any{map[string]any{"key": "ate.dev/sandboxClass", "value": "gvisor", "effect": "NoSchedule"}}},
			},
		},
	}
	if !reflect.DeepEqual(nc, want) {
		got, _ := json.MarshalIndent(nc, "", " ")
		t.Fatalf("NodeConfig =\n%s", got)
	}
}

func TestNodeUserDataWithoutLabelsOrTaints(t *testing.T) {
	ud, err := nodeUserData(testJoin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, nc := parseUserData(t, ud)
	if _, ok := nc["spec"].(map[string]any)["kubelet"].(map[string]any)["flags"]; ok {
		t.Fatalf("flags without labels: %v", nc)
	}
	if _, ok := nc["spec"].(map[string]any)["kubelet"].(map[string]any)["config"]; ok {
		t.Fatalf("kubelet config without taints: %v", nc)
	}
}

func TestNodeUserDataRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		labels map[string]string
		taints []ekstypes.Taint
	}{
		"label with a comma":   {labels: map[string]string{"a": "b,c=d"}},
		"label with a space":   {labels: map[string]string{"a b": "c"}},
		"unknown taint effect": {taints: []ekstypes.Taint{{Key: aws.String("k"), Effect: "SOMETIMES"}}},
	} {
		if _, err := nodeUserData(testJoin, c.labels, c.taints); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKubeEffect(t *testing.T) {
	for in, want := range map[ekstypes.TaintEffect]string{
		ekstypes.TaintEffectNoSchedule: "NoSchedule", ekstypes.TaintEffectPreferNoSchedule: "PreferNoSchedule", ekstypes.TaintEffectNoExecute: "NoExecute",
	} {
		if got, err := kubeEffect(in); err != nil || got != want {
			t.Errorf("kubeEffect(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNodeJoinForNeedsEveryField(t *testing.T) {
	full := func() *ekstypes.Cluster {
		return &ekstypes.Cluster{Name: aws.String("sub"), Endpoint: aws.String(testJoin.Endpoint),
			CertificateAuthority:    &ekstypes.Certificate{Data: aws.String(testJoin.CA)},
			KubernetesNetworkConfig: &ekstypes.KubernetesNetworkConfigResponse{ServiceIpv4Cidr: aws.String("10.100.0.0/16")}}
	}
	if j, err := nodeJoinFor(full()); err != nil || j != testJoin {
		t.Fatalf("nodeJoinFor = %+v, %v", j, err)
	}
	for name, drop := range map[string]func(*ekstypes.Cluster){
		"endpoint": func(c *ekstypes.Cluster) { c.Endpoint = nil },
		"CA":       func(c *ekstypes.Cluster) { c.CertificateAuthority = nil },
		"CIDR":     func(c *ekstypes.Cluster) { c.KubernetesNetworkConfig = nil },
	} {
		c := full()
		drop(c)
		if _, err := nodeJoinFor(c); err == nil {
			t.Errorf("no %s: accepted", name)
		}
	}
}
