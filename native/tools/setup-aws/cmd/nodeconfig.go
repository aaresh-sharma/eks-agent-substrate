package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// A node that EKS does not bootstrap joins the cluster with nodeadm, the
// node agent of the Amazon EKS optimized Amazon Linux 2023 AMI, from a
// NodeConfig in its user data. EKS gives no user data to a managed node
// group whose launch template names an AMI, and none to a self-managed node,
// so the launch template carries it: the cluster's name, API endpoint,
// certificate authority and service CIDR, and the node's labels and taints.
//
// The taints go in the kubelet configuration (registerWithTaints), so the
// node registers with them: EKS applies a managed node group's taints only
// after the node joins when the group names an AMI, and a sandbox Pod could
// land on the node before that. The labels go in --node-labels, which
// nodeadm appends to kubelet's flags, so an AMI's own labels (a Nitro
// Sandbox AMI's eks.amazonaws.com/nitro-sandbox=true) are kept.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html#launch-template-custom-ami
//	https://awslabs.github.io/amazon-eks-ami/nodeadm/
type nodeJoin struct {
	Cluster     string
	Endpoint    string
	CA          string // base64, as DescribeCluster returns it
	ServiceCIDR string
}

// nodeJoinFor reads a cluster's join data; it fails when the cluster does
// not report one of them yet.
func nodeJoinFor(c *ekstypes.Cluster) (nodeJoin, error) {
	j := nodeJoin{Cluster: aws.ToString(c.Name), Endpoint: aws.ToString(c.Endpoint)}
	if c.CertificateAuthority != nil {
		j.CA = aws.ToString(c.CertificateAuthority.Data)
	}
	if c.KubernetesNetworkConfig != nil {
		j.ServiceCIDR = aws.ToString(c.KubernetesNetworkConfig.ServiceIpv4Cidr)
	}
	switch {
	case j.Cluster == "":
		return j, fmt.Errorf("the cluster has no name")
	case j.Endpoint == "":
		return j, fmt.Errorf("cluster %s reports no API endpoint yet", j.Cluster)
	case j.CA == "":
		return j, fmt.Errorf("cluster %s reports no certificate authority yet", j.Cluster)
	case j.ServiceCIDR == "":
		return j, fmt.Errorf("cluster %s reports no IPv4 service CIDR (an IPv6 cluster is not supported)", j.Cluster)
	}
	return j, nil
}

// nodeConfig is nodeadm's NodeConfig (node.eks.aws/v1alpha1), as JSON,
// which nodeadm reads as YAML.
type nodeConfig struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Spec       nodeConfigSpec `json:"spec"`
}

type nodeConfigSpec struct {
	Cluster nodeConfigCluster `json:"cluster"`
	Kubelet nodeConfigKubelet `json:"kubelet"`
}

type nodeConfigCluster struct {
	Name                 string `json:"name"`
	APIServerEndpoint    string `json:"apiServerEndpoint"`
	CertificateAuthority string `json:"certificateAuthority"`
	CIDR                 string `json:"cidr"`
}

type nodeConfigKubelet struct {
	Config map[string]any `json:"config,omitempty"`
	Flags  []string       `json:"flags,omitempty"`
}

// kubeTaint is a taint as the kubelet configuration's registerWithTaints
// takes it.
type kubeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// kubeEffect is an EKS API taint effect as Kubernetes spells it.
func kubeEffect(e ekstypes.TaintEffect) (string, error) {
	switch e {
	case ekstypes.TaintEffectNoSchedule:
		return "NoSchedule", nil
	case ekstypes.TaintEffectPreferNoSchedule:
		return "PreferNoSchedule", nil
	case ekstypes.TaintEffectNoExecute:
		return "NoExecute", nil
	}
	return "", fmt.Errorf("unknown taint effect %q", e)
}

// userDataBoundary separates the parts of the MIME user data. It cannot
// appear at the start of a line of the NodeConfig, which is one JSON line.
const userDataBoundary = "==SUBSTRATE-EKS-NODE=="

// nodeUserData is the launch template's user data (base64) for a node of a
// group with labels and taints: a MIME multi-part document whose one part
// is the NodeConfig, the format EKS asks for in a managed node group's
// launch template and that nodeadm reads on a self-managed node.
func nodeUserData(j nodeJoin, labels map[string]string, taints []ekstypes.Taint) (string, error) {
	nc := nodeConfig{APIVersion: "node.eks.aws/v1alpha1", Kind: "NodeConfig", Spec: nodeConfigSpec{
		Cluster: nodeConfigCluster{Name: j.Cluster, APIServerEndpoint: j.Endpoint, CertificateAuthority: j.CA, CIDR: j.ServiceCIDR}}}
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		kv := make([]string, 0, len(keys))
		for _, k := range keys {
			if strings.ContainsAny(k+labels[k], ",= \n") {
				return "", fmt.Errorf("node label %s=%s cannot go in --node-labels", k, labels[k])
			}
			kv = append(kv, k+"="+labels[k])
		}
		nc.Spec.Kubelet.Flags = []string{"--node-labels=" + strings.Join(kv, ",")}
	}
	if len(taints) > 0 {
		var ts []kubeTaint
		for _, t := range taints {
			eff, err := kubeEffect(t.Effect)
			if err != nil {
				return "", err
			}
			ts = append(ts, kubeTaint{Key: aws.ToString(t.Key), Value: aws.ToString(t.Value), Effect: eff})
		}
		nc.Spec.Kubelet.Config = map[string]any{"registerWithTaints": ts}
	}
	body, err := json.Marshal(nc)
	if err != nil {
		return "", err
	}
	doc := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + userDataBoundary + "\"\r\n" +
		"\r\n" +
		"--" + userDataBoundary + "\r\n" +
		"Content-Type: application/node.eks.aws\r\n" +
		"\r\n" +
		string(body) + "\r\n" +
		"\r\n" +
		"--" + userDataBoundary + "--\r\n"
	return base64.StdEncoding.EncodeToString([]byte(doc)), nil
}
