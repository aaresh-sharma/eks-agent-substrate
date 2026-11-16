package mode3iso

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// obj is a Kubernetes object as `kubectl get -o json` prints it.
type obj = map[string]any

// child returns m[key] as an object, making it when missing; an existing
// value of another type is an error.
func child(m obj, key string) (obj, error) {
	v, ok := m[key]
	if !ok || v == nil {
		c := obj{}
		m[key] = c
		return c, nil
	}
	c, ok := v.(obj)
	if !ok {
		return nil, fmt.Errorf("%s is %T, not an object", key, v)
	}
	return c, nil
}

// path walks child through keys.
func path(m obj, keys ...string) (obj, error) {
	var err error
	for _, k := range keys {
		if m, err = child(m, k); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// NodePool is the NodePool ate-isolated, made from
// Auto Mode's built-in general-purpose NodePool (`kubectl get nodepool
// general-purpose -o json`): the same requirements, its nodes labeled
// ate.dev/pool=isolated and tainted ate.dev/sandboxClass=isolated, so only the
// worker Pods and atelet-iso run there. An empty node is kept a few minutes,
// so a resume rarely waits for a new one. Its nodes come from NodeClass
// ate-isolated (NodeClass), which tags them; NodePool also returns the name
// of the NodeClass general-purpose uses, which that one copies.
func NodePool(generalPurpose []byte) (np []byte, nodeClass string, err error) {
	var gp obj
	if err := json.Unmarshal(generalPurpose, &gp); err != nil {
		return nil, "", fmt.Errorf("read NodePool general-purpose: %w", err)
	}
	if gp["kind"] != "NodePool" {
		return nil, "", fmt.Errorf("general-purpose is a %v, not a NodePool", gp["kind"])
	}
	spec, ok := gp["spec"].(obj)
	if !ok {
		return nil, "", errors.New("NodePool general-purpose has no spec")
	}
	labels, err := path(spec, "template", "metadata", "labels")
	if err != nil {
		return nil, "", fmt.Errorf("NodePool general-purpose: %w", err)
	}
	labels[PoolLabel] = PoolValue
	tspec, err := path(spec, "template", "spec")
	if err != nil {
		return nil, "", fmt.Errorf("NodePool general-purpose: %w", err)
	}
	tspec["taints"] = []any{obj{"key": TaintKey, "value": PoolValue, "effect": "NoSchedule"}}
	ref, err := child(tspec, "nodeClassRef")
	if err != nil {
		return nil, "", fmt.Errorf("NodePool general-purpose: %w", err)
	}
	nodeClass, _ = ref["name"].(string)
	if nodeClass == "" {
		return nil, "", errors.New("NodePool general-purpose names no NodeClass")
	}
	ref["name"] = NodePoolName
	spec["disruption"] = obj{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "300s", "budgets": []any{obj{"nodes": "10%"}}}
	np, err = json.Marshal(obj{"apiVersion": gp["apiVersion"], "kind": "NodePool",
		"metadata": obj{"name": NodePoolName}, "spec": spec})
	return np, nodeClass, err
}

// archKey is the node label a NodePool requirement names to choose the CPU
// architecture of the nodes it launches.
const archKey = "kubernetes.io/arch"

// PoolArch is the one CPU architecture NodePool ate-isolated (`kubectl get
// nodepool ate-isolated -o json`) lets Auto Mode launch, which the counter
// image is built for: the values its kubernetes.io/arch requirements, all
// with operator In, leave between them. A NodePool that allows more than
// one architecture, any (no such requirement, or another operator), or
// none is refused.
//
// shortcut: one single-architecture image, as the demo builds; push an
// index if the isolated nodes ever need two architectures.
func PoolArch(nodePool []byte) (string, error) {
	var np struct {
		Spec struct {
			Template struct {
				Spec struct {
					Requirements []struct {
						Key      string   `json:"key"`
						Operator string   `json:"operator"`
						Values   []string `json:"values"`
					} `json:"requirements"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(nodePool, &np); err != nil {
		return "", fmt.Errorf("read NodePool %s: %w", NodePoolName, err)
	}
	why := func(s string) error {
		return fmt.Errorf("NodePool %s %s: mode3-iso builds its counter image for one architecture, so the NodePool must allow exactly one (%s In [amd64] or [arm64])",
			NodePoolName, s, archKey)
	}
	var allowed []string
	seen := false
	for _, r := range np.Spec.Template.Spec.Requirements {
		if r.Key != archKey {
			continue
		}
		if r.Operator != "In" {
			return "", why(fmt.Sprintf("requires %s %s %v, which allows more than one architecture", archKey, r.Operator, r.Values))
		}
		if !seen {
			allowed, seen = slices.Clone(r.Values), true
			continue
		}
		allowed = slices.DeleteFunc(allowed, func(v string) bool { return !slices.Contains(r.Values, v) })
	}
	allowed = slices.Compact(slices.Sorted(slices.Values(allowed)))
	switch {
	case !seen:
		return "", why("has no " + archKey + " requirement, so it allows every architecture")
	case len(allowed) == 0:
		return "", why("allows no architecture")
	case len(allowed) > 1:
		return "", why("allows " + strings.Join(allowed, " and "))
	}
	return allowed[0], nil
}

// NodeClass is NodeClass ate-isolated: a copy of the Auto Mode NodeClass
// general-purpose uses (`kubectl get nodeclass <name> -o json`), with the
// same node role, subnets and security groups, that adds tags (the
// deployment's owner tag and TAGS) to the EC2 instances and volumes Auto Mode
// launches for NodePool ate-isolated. Tags the source sets are kept unless
// tags names them.
func NodeClass(source []byte, tags map[string]string) ([]byte, error) {
	var nc obj
	if err := json.Unmarshal(source, &nc); err != nil {
		return nil, fmt.Errorf("read the NodeClass: %w", err)
	}
	if nc["kind"] != "NodeClass" {
		return nil, fmt.Errorf("the NodeClass is a %v", nc["kind"])
	}
	spec, ok := nc["spec"].(obj)
	if !ok {
		return nil, errors.New("the NodeClass has no spec")
	}
	t, err := child(spec, "tags")
	if err != nil {
		return nil, fmt.Errorf("the NodeClass: %w", err)
	}
	for k, v := range tags {
		t[k] = v
	}
	return json.Marshal(obj{"apiVersion": nc["apiVersion"], "kind": "NodeClass",
		"metadata": obj{"name": NodePoolName}, "spec": spec})
}

// The atelet volumes that hold the kubelet's image credential provider: its
// binaries and its config. deploy/eks points them at the AL2023 paths.
const (
	providerBin    = "image-credential-provider-bin"
	providerConfig = "image-credential-provider-config"
)

// AteletIso is atelet-iso, made from the atelet
// DaemonSet (`kubectl -n ate-system get ds atelet -o json`): the same Pods,
// labeled ate.dev/atelet-variant=iso and kept app=atelet (so the ate-system
// NetworkPolicies cover them), on the isolated nodes only, with the
// Bottlerocket path of the ECR credential provider and the provider config of
// ConfigMap atelet-iso-credential-provider. It is a copy, so a Substrate
// upgrade that changes atelet needs the plugin deployed again.
func AteletIso(atelet []byte) ([]byte, error) {
	var d obj
	if err := json.Unmarshal(atelet, &d); err != nil {
		return nil, fmt.Errorf("read DaemonSet atelet: %w", err)
	}
	if d["kind"] != "DaemonSet" {
		return nil, fmt.Errorf("atelet is a %v, not a DaemonSet", d["kind"])
	}
	m, ok := d["metadata"].(obj)
	if !ok {
		return nil, errors.New("DaemonSet atelet has no metadata")
	}
	meta := obj{"name": AteletName, "namespace": m["namespace"]}
	if l, ok := m["labels"]; ok {
		meta["labels"] = l
	}
	d["metadata"] = meta
	delete(d, "status")
	spec, ok := d["spec"].(obj)
	if !ok {
		return nil, errors.New("DaemonSet atelet has no spec")
	}
	sel, err := path(spec, "selector", "matchLabels")
	if err != nil {
		return nil, fmt.Errorf("DaemonSet atelet: %w", err)
	}
	sel[VariantLabel] = "iso"
	tl, err := path(spec, "template", "metadata", "labels")
	if err != nil {
		return nil, fmt.Errorf("DaemonSet atelet: %w", err)
	}
	tl[VariantLabel] = "iso"
	p, err := path(spec, "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("DaemonSet atelet: %w", err)
	}
	p["affinity"] = obj{"nodeAffinity": obj{"requiredDuringSchedulingIgnoredDuringExecution": obj{"nodeSelectorTerms": []any{
		obj{"matchExpressions": []any{obj{"key": PoolLabel, "operator": "In", "values": []any{PoolValue}}}}}}}}
	vols, _ := p["volumes"].([]any)
	found := map[string]bool{}
	for _, v := range vols {
		vol, ok := v.(obj)
		if !ok {
			continue
		}
		switch vol["name"] {
		case providerBin:
			vol["hostPath"] = obj{"path": "/usr/libexec/kubernetes/kubelet/plugins", "type": "Directory"}
			found[providerBin] = true
		case providerConfig:
			delete(vol, "hostPath")
			vol["configMap"] = obj{"name": CredentialProviderConfigMap}
			found[providerConfig] = true
		}
	}
	if !found[providerBin] || !found[providerConfig] {
		return nil, fmt.Errorf("DaemonSet atelet has no %s and %s volumes (found %v)", providerBin, providerConfig, found)
	}
	cs, _ := p["containers"].([]any)
	for _, c := range cs {
		ct, _ := c.(obj)
		mounts, _ := ct["volumeMounts"].([]any)
		for _, vm := range mounts {
			if mt, ok := vm.(obj); ok && mt["name"] == providerConfig {
				mt["subPath"] = "config.yaml"
			}
		}
	}
	return json.Marshal(d)
}
