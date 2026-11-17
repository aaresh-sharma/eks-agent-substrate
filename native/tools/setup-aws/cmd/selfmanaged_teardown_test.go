package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

const tdNodeRoleARN = "arn:aws:iam::111122223333:role/demo-us-west-2-eks-node"

// selfManagedDeployment is fullDeployment with self-managed node groups:
// instead of the managed node groups, an Auto Scaling group per group with
// its instance, the node role in its owned instance profile, and the role's
// owned EC2_LINUX access entry.
func selfManagedDeployment() *tdFake {
	f := fullDeployment()
	for k, o := range f.objs {
		if o.kind == "nodegroup" || o.tags["eks:nodegroup-name"] != "" {
			delete(f.objs, k)
		}
	}
	f.objs["role|demo-us-west-2-eks-node"].profiles = []string{"demo-us-west-2-eks-node"}
	f.put(&tdObj{kind: "profile", id: "demo-us-west-2-eks-node", tags: own()})
	f.put(&tdObj{kind: "access entry", id: tdNodeRoleARN, tags: own()})
	for _, g := range nodegroups.All {
		f.put(&tdObj{kind: "asg", id: "demo-" + g, tags: own("Name", "demo-"+g, "kubernetes.io/cluster/demo", "owned")})
		f.put(&tdObj{kind: kInstance, id: "i-" + g, vpc: "vpc-1", state: "running", parent: "asg|demo-" + g, tags: own("Name", "demo-"+g)})
	}
	// The tagging API lists an Auto Scaling group too, and keeps listing it
	// a while after its delete.
	f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{
		ResourceARN: aws.String("arn:aws:autoscaling:us-west-2:111122223333:autoScalingGroup:0002:autoScalingGroupName/demo-system"),
		Tags:        []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
	return f
}

// The tagging API's Auto Scaling groups are not rows of their own: Auto
// Scaling's answer lists a self-managed group, and EKS's own group of a
// managed node group goes with its node group. So a managed deployment's
// rows are what they were, and --keep-cluster's final check passes on both.
func TestInventoryTaggingAPIAutoScalingGroups(t *testing.T) {
	for name, f := range map[string]*tdFake{"managed": fullDeployment(), "self-managed": selfManagedDeployment()} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := tdTestEnv(t, f, false, false)
			rows, err := e.inventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range rows {
				if strings.HasPrefix(r.Kind, "autoscaling") || strings.Contains(r.ID, "eks-system-0001") {
					t.Errorf("a tagging-API row %s %s", r.Kind, r.ID)
				}
				if r.Kind == kASG {
					n++
				}
			}
			if want := map[string]int{"managed": 0, "self-managed": 3}[name]; n != want {
				t.Errorf("%d Auto Scaling group rows, want %d", n, want)
			}
			e, _, logs := tdTestEnv(t, f, false, true)
			if err := e.teardown(context.Background()); err != nil {
				t.Errorf("teardown --keep-cluster: %v\n%s", err, logs)
			}
		})
	}
}

// Inventory lists the Auto Scaling groups, the instance profile and the
// node role's access entry, as cluster resources.
func TestInventoryListsSelfManagedNodeGroups(t *testing.T) {
	e, _, _ := tdTestEnv(t, selfManagedDeployment(), false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Auto Scaling group demo-system": true, "Auto Scaling group demo-gvisor": true, "Auto Scaling group demo-microvm": true,
		"IAM instance profile demo-us-west-2-eks-node": true, "EKS access entry " + tdNodeRoleARN: true}
	for _, r := range rows {
		k := r.Kind + " " + r.ID
		if want[k] {
			if !r.Cluster {
				t.Errorf("%s is not marked a cluster resource", k)
			}
			delete(want, k)
		}
	}
	for k := range want {
		t.Errorf("inventory lacks %q", k)
	}
}

// A full teardown deletes the Auto Scaling groups (with their instances)
// before the cluster and the launch templates, the access entry, and the
// instance profile once the role is out of it; nothing is left after.
func TestTeardownDeletesSelfManagedNodeGroups(t *testing.T) {
	f := selfManagedDeployment()
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	want := []string{"NAT gateway|nat-1", "VPC|vpc-default", "repo|other/app", "role|demo-2-us-west-2-node", "role|someone-elses"}
	if got := f.remaining(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("left after teardown:\n got %v\nwant %v", got, want)
	}
	c := f.calls
	for _, g := range nodegroups.All {
		before(t, c, "DeleteAutoScalingGroup demo-"+g, "DeleteCluster demo")
		before(t, c, "DeleteAutoScalingGroup demo-"+g, "DeleteLaunchTemplate lt-1")
	}
	before(t, c, "DeleteAutoScalingGroup demo-microvm", "DeleteAccessEntry "+tdNodeRoleARN)
	before(t, c, "DeleteAccessEntry "+tdNodeRoleARN, "DeleteCluster demo")
	before(t, c, "RemoveRoleFromInstanceProfile demo-us-west-2-eks-node", "DeleteRole demo-us-west-2-eks-node")
	before(t, c, "DeleteRole demo-us-west-2-eks-node", "DeleteInstanceProfile demo-us-west-2-eks-node")
	before(t, c, "DeleteInstanceProfile demo-us-west-2-eks-node", "DeleteVpc vpc-1")
	if countCalls(c, "DeleteVpc vpc-1") != 1 {
		t.Errorf("the VPC delete was retried: an instance held it")
	}
	phaseOrder(t, logs.String())
	e, _, _ = tdTestEnv(t, f, false, false)
	if rows, err := e.inventory(context.Background()); err != nil || len(rows) != 0 {
		t.Errorf("inventory after: %+v, %v; want nothing left", rows, err)
	}
	// A second run finds nothing to delete.
	f.calls = nil
	e, _, _ = tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil || len(f.calls) != 0 {
		t.Errorf("second run: %v, writes %v", err, f.calls)
	}
}

// --keep-cluster keeps them, and its final check passes.
func TestTeardownKeepClusterKeepsSelfManagedNodeGroups(t *testing.T) {
	f := selfManagedDeployment()
	e, _, logs := tdTestEnv(t, f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown --keep-cluster: %v\n%s", err, logs)
	}
	for _, kept := range []string{"asg|demo-system", "asg|demo-gvisor", "asg|demo-microvm", kInstance + "|i-gvisor",
		"profile|demo-us-west-2-eks-node", "access entry|" + tdNodeRoleARN} {
		if f.objs[kept] == nil {
			t.Errorf("--keep-cluster deleted %s", kept)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DeleteAutoScalingGroup") || strings.HasPrefix(c, "DeleteAccessEntry") || strings.HasPrefix(c, "DeleteInstanceProfile") {
			t.Errorf("--keep-cluster ran %s", c)
		}
	}
}

// An Auto Scaling group of a derived name with another owner stops the run
// before anything is deleted; --keep-cluster does not reach it.
func TestTeardownRefusesAForeignAutoScalingGroup(t *testing.T) {
	foreign := func() *tdFake {
		f := selfManagedDeployment()
		f.objs["asg|demo-gvisor"].tags = map[string]string{OwnerTagKey: "other.us-west-2"}
		delete(f.objs, kInstance+"|i-gvisor")
		return f
	}
	f := foreign()
	e, _, _ := tdTestEnv(t, f, false, false)
	err := e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Auto Scaling group demo-gvisor is not owned by demo.us-west-2") {
		t.Fatalf("want a refusal naming demo-gvisor, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("deleted before refusing: %v", f.calls)
	}
	f = foreign()
	e, _, _ = tdTestEnv(t, f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Errorf("--keep-cluster stopped on a cluster resource it keeps: %v", err)
	}
}

// Inventory lists an Auto Scaling group of a derived name whatever its tags,
// with its owner, as launch templates are, so the table teardown prints
// before it refuses one shows it.
func TestInventoryListsAutoScalingGroupsOfADerivedName(t *testing.T) {
	f := fullDeployment()
	f.put(&tdObj{kind: "asg", id: "demo-gvisor", tags: map[string]string{OwnerTagKey: "other.us-west-2"}})
	f.put(&tdObj{kind: "asg", id: "demo-microvm", tags: map[string]string{}})
	f.put(&tdObj{kind: "asg", id: "other-system", tags: map[string]string{OwnerTagKey: "other.us-west-2"}})
	e, out, _ := tdTestEnv(t, f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"demo-gvisor", "demo-microvm"} {
		if !hasRow(rows, "Auto Scaling group "+id) {
			t.Errorf("inventory lacks the Auto Scaling group %s", id)
		}
	}
	if hasRow(rows, "Auto Scaling group other-system") {
		t.Error("inventory lists another deployment's Auto Scaling group of another name")
	}
	e.printRows(rows)
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "Auto Scaling group") {
			lines = append(lines, strings.Join(strings.Fields(l), " "))
		}
	}
	if want := []string{"Auto Scaling group demo-gvisor other.us-west-2 -", "Auto Scaling group demo-microvm - -"}; strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("rows %q, want %q", lines, want)
	}
	if err := e.teardown(context.Background()); err == nil || !strings.Contains(err.Error(), "Auto Scaling group demo-gvisor is not owned") ||
		!strings.Contains(err.Error(), "Auto Scaling group demo-microvm is not owned") || len(f.calls) != 0 {
		t.Errorf("teardown: %v, writes %v; want both refused before any delete", err, f.calls)
	}
}

// IAM can still see the role in the instance profile for a moment after it
// left it: DeleteConflict is retried until the profile goes.
func TestTeardownRetriesTheInstanceProfileDeleteConflict(t *testing.T) {
	f := selfManagedDeployment()
	f.objs["profile|demo-us-west-2-eks-node"].conflicts = 2
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	if n := countCalls(f.calls, "DeleteInstanceProfile demo-us-west-2-eks-node"); n != 3 || f.objs["profile|demo-us-west-2-eks-node"] != nil {
		t.Errorf("DeleteInstanceProfile %d times, want 3; profile left %v", n, f.objs["profile|demo-us-west-2-eks-node"] != nil)
	}
}

// An owned instance profile that still holds a role teardown keeps (one
// without the owner tag) loses the role, then goes; the role stays.
func TestTeardownEmptiesTheInstanceProfile(t *testing.T) {
	f := selfManagedDeployment()
	f.objs["role|demo-us-west-2-eks-node"].tags = map[string]string{}
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	before(t, f.calls, "RemoveRoleFromInstanceProfile demo-us-west-2-eks-node", "DeleteInstanceProfile demo-us-west-2-eks-node")
	if f.objs["profile|demo-us-west-2-eks-node"] != nil || f.objs["role|demo-us-west-2-eks-node"] == nil {
		t.Errorf("profile left %v, role kept %v", f.objs["profile|demo-us-west-2-eks-node"] != nil, f.objs["role|demo-us-west-2-eks-node"] != nil)
	}
}

// An Auto Scaling group AWS is already deleting is waited on, not deleted
// again; one deleted behind teardown's back is not an error.
func TestTeardownAutoScalingGroupsBeingDeletedOrGone(t *testing.T) {
	f := selfManagedDeployment()
	// Gone only after the inventory and both teardown passes have read it.
	f.objs["asg|demo-gvisor"].deleting, f.objs["asg|demo-gvisor"].pending = true, 12
	f.vanish["DeleteAutoScalingGroup"] = true
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	if countCalls(f.calls, "DeleteAutoScalingGroup demo-gvisor") != 0 {
		t.Errorf("deleted an Auto Scaling group AWS was deleting: %v", f.calls)
	}
	if f.objs["asg|demo-gvisor"] != nil || f.objs["asg|demo-system"] != nil {
		t.Error("an Auto Scaling group is left")
	}
}

// What is not this deployment's is left and not listed: the access entry EKS
// makes for a managed node group's role (no owner tag), an instance profile
// without the owner tag, and EKS's own Auto Scaling groups of managed node
// groups, which create cluster tags with the owner tag.
func TestTeardownLeavesWhatIsNotItsOwn(t *testing.T) {
	f := fullDeployment()
	f.put(&tdObj{kind: "access entry", id: tdNodeRoleARN, tags: map[string]string{}})
	f.put(&tdObj{kind: "profile", id: "demo-us-west-2-eks-node", tags: map[string]string{}})
	f.objs["role|demo-us-west-2-eks-node"].profiles = []string{"demo-us-west-2-eks-node"}
	for _, ng := range nodegroups.All {
		f.put(&tdObj{kind: "asg", id: "eks-" + ng + "-0001", tags: own("eks:nodegroup-name", ng)})
	}
	e, _, logs := tdTestEnv(t, f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Kind == kASG || r.Kind == kProfile || r.Kind == kAccessEntry {
			t.Errorf("listed %s %s", r.Kind, r.ID)
		}
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DeleteAutoScalingGroup") || strings.HasPrefix(c, "DeleteAccessEntry") || strings.HasPrefix(c, "DeleteInstanceProfile") {
			t.Errorf("deleted what is not its own: %s", c)
		}
	}
	if f.objs["profile|demo-us-west-2-eks-node"] == nil {
		t.Error("deleted an instance profile without the owner tag")
	}
}

// A cluster that authenticates with the aws-auth ConfigMap only (an older
// eksctl cluster) has no access entries, and EKS refuses to read one there:
// inventory and teardown do not ask.
func TestInventoryOnAConfigMapCluster(t *testing.T) {
	f := fullDeployment()
	f.objs["cluster|demo"].authMode = "CONFIG_MAP"
	e, _, logs := tdTestEnv(t, f, false, true)
	if _, err := e.inventory(context.Background()); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown --keep-cluster: %v\n%s", err, logs)
	}
	// A full teardown runs the cluster phases, the node access entry's
	// step among them, on the same cluster.
	f = fullDeployment()
	f.objs["cluster|demo"].authMode = "CONFIG_MAP"
	e, _, logs = tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	if f.objs["cluster|demo"] != nil || countCalls(f.calls, "DeleteCluster demo") != 1 {
		t.Errorf("the cluster is left (%d deletes)", countCalls(f.calls, "DeleteCluster demo"))
	}
}

// A denied read of a self-managed resource stops the inventory.
func TestInventorySelfManagedDeniedReadIsAnError(t *testing.T) {
	for _, op := range []string{"DescribeAutoScalingGroups", "GetInstanceProfile", "DescribeAccessEntry"} {
		f := selfManagedDeployment()
		f.deny[op] = true
		e, _, _ := tdTestEnv(t, f, false, false)
		if _, err := e.inventory(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
			t.Errorf("%s denied: %v", op, err)
		}
		if err := e.teardown(context.Background()); err == nil || len(f.calls) != 0 {
			t.Errorf("%s denied: teardown %v, writes %v", op, err, f.calls)
		}
	}
}
