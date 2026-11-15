package openclaw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// intendedPolicy is the one rule LLMPolicy may hold, as kubectl prints its
// spec: pods labeled app=openclaw-gateway in namespace openclaw (one peer,
// both selectors) to the LLM gateway's pods on TCP 4000.
const intendedPolicy = `{"podSelector":{"matchLabels":{"app":"llm-gateway"}},"policyTypes":["Ingress"],` +
	`"ingress":[{"from":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"openclaw"}},` +
	`"podSelector":{"matchLabels":{"app":"openclaw-gateway"}}}],"ports":[{"port":4000,"protocol":"TCP"}]}]}`

// widerPolicies each admit more than the gateway's pods, or other ports.
var widerPolicies = map[string]string{
	"the selectors as two peers (either matches)": strings.Replace(intendedPolicy,
		`"openclaw"}},"podSelector"`, `"openclaw"}}},{"podSelector"`, 1),
	"the whole openclaw namespace (its workers too)": strings.Replace(intendedPolicy,
		`,"podSelector":{"matchLabels":{"app":"openclaw-gateway"}}`, "", 1),
	"gateway pods in any namespace": strings.Replace(intendedPolicy,
		`{"matchLabels":{"kubernetes.io/metadata.name":"openclaw"}}`, `{}`, 1),
	"another port too": strings.Replace(intendedPolicy,
		`[{"port":4000,"protocol":"TCP"}]`, `[{"port":4000,"protocol":"TCP"},{"port":4001,"protocol":"TCP"}]`, 1),
	"every port":                  strings.Replace(intendedPolicy, `,"ports":[{"port":4000,"protocol":"TCP"}]`, "", 1),
	"a second rule from anywhere": strings.Replace(intendedPolicy, `}]}]}`, `}]},{}]}`, 1),
	"every pod of llm-gateway":    strings.Replace(intendedPolicy, `{"podSelector":{"matchLabels":{"app":"llm-gateway"}}`, `{"podSelector":{}`, 1),
	"no policy":                   "",
}

// Both ways: llm-gateway-policy.yaml is exactly the intended rule, and
// samePolicy, Verify's check, refuses each policy that admits more.
func TestLLMPolicyAdmitsOnlyTheGatewayPods(t *testing.T) {
	if err := samePolicy(intendedPolicy); err != nil {
		t.Fatalf("llm-gateway-policy.yaml is not the intended rule: %v", err)
	}
	for name, p := range widerPolicies {
		if p == intendedPolicy {
			t.Fatalf("%s: the variant did not change the policy", name)
		}
		if err := samePolicy(p); err == nil {
			t.Errorf("%s: samePolicy accepted %s", name, p)
		}
	}
	n, err := yaml.Parse(llmPolicyYAML)
	if err != nil {
		t.Fatal(err)
	}
	if n.GetKind() != "NetworkPolicy" || n.GetName() != LLMPolicy || n.GetNamespace() != llmNamespace {
		t.Errorf("llm-gateway-policy.yaml is %s %s/%s, want NetworkPolicy %s/%s", n.GetKind(), n.GetNamespace(), n.GetName(), llmNamespace, LLMPolicy)
	}
}

// docs are the YAML documents of a manifest, by kind and name.
func docs(t *testing.T, manifest string) map[string]*yaml.RNode {
	t.Helper()
	nodes, err := kio.FromBytes([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*yaml.RNode{}
	for _, n := range nodes {
		m[n.GetKind()+"/"+n.GetName()] = n
	}
	return m
}

func field(t *testing.T, n *yaml.RNode, path ...string) string {
	t.Helper()
	if n == nil {
		t.Fatalf("no object for %v", path)
	}
	v, err := n.Pipe(yaml.Lookup(path...))
	if err != nil || v == nil {
		t.Fatalf("%s has no %v: %v", n.GetName(), path, err)
	}
	if v.YNode().Kind == yaml.ScalarNode {
		return v.YNode().Value
	}
	b, err := v.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// selects reports whether a matchLabels selector matches a pod with labels:
// every entry of sel is in labels.
func selects(sel, labels map[string]string) bool {
	for k, v := range sel {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// The policy names what the other manifests make: the gateway Deployment's
// pod labels and namespace, and the LLM gateway's pods and the port its
// Service sends to.
func TestLLMPolicyNamesTheGatewayAndTheLLMGateway(t *testing.T) {
	var spec struct {
		PodSelector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"podSelector"`
		Ingress []struct {
			From []struct {
				NamespaceSelector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"namespaceSelector"`
				PodSelector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"podSelector"`
			} `json:"from"`
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"ingress"`
	}
	if err := json.Unmarshal([]byte(intendedPolicy), &spec); err != nil {
		t.Fatal(err)
	}
	peer := spec.Ingress[0].From[0]

	gw := docs(t, gatewayYAML)["Deployment/openclaw-gateway"]
	var labels map[string]string
	if err := json.Unmarshal([]byte(field(t, gw, "spec", "template", "metadata", "labels")), &labels); err != nil {
		t.Fatal(err)
	}
	for k, v := range peer.PodSelector.MatchLabels {
		if labels[k] != v {
			t.Errorf("the gateway's pods are labeled %v; the policy selects %s=%s", labels, k, v)
		}
	}
	if ns := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ns != Namespace || gw.GetNamespace() != Namespace {
		t.Errorf("the policy admits namespace %q, the gateway runs in %q, want %q", ns, gw.GetNamespace(), Namespace)
	}

	b, err := os.ReadFile(filepath.Join("..", "llmgateway", "llm-gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	llm := docs(t, string(b))
	var llmLabels map[string]string
	if err := json.Unmarshal([]byte(field(t, llm["Deployment/llm-gateway"], "spec", "template", "metadata", "labels")), &llmLabels); err != nil {
		t.Fatal(err)
	}
	if !selects(spec.PodSelector.MatchLabels, llmLabels) {
		t.Errorf("the LLM gateway's pods are labeled %v; the policy applies to pods labeled %v", llmLabels, spec.PodSelector.MatchLabels)
	}
	if got, want := field(t, llm["NetworkPolicy/llm-gateway-ingress"], "spec", "podSelector"), `{"matchLabels":{"app":"llm-gateway"}}`; got != want {
		t.Errorf("llm-gateway-ingress selects %s, want %s", got, want)
	}
	type containerPort struct {
		Name          string `json:"name"`
		ContainerPort int    `json:"containerPort"`
	}
	var ports []containerPort
	if err := json.Unmarshal([]byte(field(t, llm["Deployment/llm-gateway"], "spec", "template", "spec", "containers", "[name=agentgateway]", "ports")), &ports); err != nil {
		t.Fatal(err)
	}
	target := field(t, llm["Service/llm-gateway"], "spec", "ports", "[name=http]", "targetPort")
	if !slices.ContainsFunc(ports, func(p containerPort) bool {
		return p.Name == target && p.ContainerPort == spec.Ingress[0].Ports[0].Port
	}) {
		t.Errorf("the LLM gateway's Service sends to %s, its ports are %+v; the policy admits %d", target, ports, spec.Ingress[0].Ports[0].Port)
	}
}

// Both ways: with the LLM gateway there, Deploy applies the policy; without
// it, Deploy says so and applies none.
func TestDeployAppliesTheLLMPolicy(t *testing.T) {
	out, stdins, err := fakeDeploy(t, map[string]string{"kubectl --kubeconfig /kc get namespace llm-gateway": "namespace/llm-gateway"})
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !slices.Contains(stdins, llmPolicyYAML) || !strings.Contains(out, "NetworkPolicy llm-gateway/llm-gateway-from-openclaw: the gateway's pods reach the LLM gateway") {
		t.Errorf("with the LLM gateway, deploy did not apply %s:\n%s", LLMPolicy, out)
	}
	out, stdins, err = fakeDeploy(t, nil)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if slices.Contains(stdins, llmPolicyYAML) || !strings.Contains(out, "No namespace llm-gateway: skipping NetworkPolicy llm-gateway-from-openclaw") {
		t.Errorf("without the LLM gateway, deploy applied %s or did not say it skipped it:\n%s", LLMPolicy, out)
	}
}

// Verify passes the policy check on the intended policy only, and skips it
// with a note when there is no LLM gateway. Each case then stops at the
// image check, which the fake does not answer.
func TestVerifyChecksTheLLMPolicy(t *testing.T) {
	for _, k := range imageVars {
		t.Setenv(k, "")
	}
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc"}
	kc := "kubectl --kubeconfig /kc "
	const pass = "PASS: NetworkPolicy llm-gateway/llm-gateway-from-openclaw admits only the gateway's pods to the LLM gateway, on TCP 4000"
	run := func(namespace bool, live string) (string, error) {
		answers := map[string]string{
			kc + "-n ate-system get deployment atenet-router":                                                GatewayID + " " + DashboardID,
			kc + "-n openclaw get services":                                                                  `{"items":[{"metadata":{"name":"s0"},"spec":{"type":"ClusterIP"}}]}`,
			kc + "-n llm-gateway get networkpolicy " + LLMPolicy + " --ignore-not-found -o jsonpath={.spec}": live,
		}
		if namespace {
			answers[kc+"get namespace llm-gateway"] = "namespace/llm-gateway"
		}
		fakeExec(t, answers)
		var out strings.Builder
		err := verify(context.Background(), &out, o)
		return out.String(), err
	}
	imageErr := "not the pinned " + GatewayImage
	out, err := run(true, intendedPolicy)
	if !strings.Contains(out, pass) || err == nil || !strings.Contains(err.Error(), imageErr) {
		t.Errorf("the intended policy: %v\n%s", err, out)
	}
	for name, p := range widerPolicies {
		out, err := run(true, p)
		if strings.Contains(out, pass) || err == nil || !strings.Contains(err.Error(), LLMPolicy) || !strings.Contains(err.Error(), "deploy the openclaw plugin again") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	out, err = run(false, "")
	if strings.Contains(out, pass) || !strings.Contains(out, "No namespace llm-gateway") || err == nil || !strings.Contains(err.Error(), imageErr) {
		t.Errorf("no LLM gateway: %v\n%s", err, out)
	}
}

// Remove deletes the policy from the LLM gateway's namespace.
func TestRemoveDeletesTheLLMPolicy(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", Release: plugin.Release{Tag: "v1.0.0", Tree: tree}}
	log := fakeExec(t, nil)
	if err := remove(context.Background(), &strings.Builder{}, o); err != nil {
		t.Fatal(err)
	}
	want := "kubectl --kubeconfig /kc -n llm-gateway delete networkpolicy llm-gateway-from-openclaw --ignore-not-found"
	if !slices.Contains(*log, want) {
		t.Errorf("remove did not run %q; it ran:\n%s", want, strings.Join(*log, "\n"))
	}
}
