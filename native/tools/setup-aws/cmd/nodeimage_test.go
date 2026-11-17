package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const testAMI = "ami-0123456789abcdef0"

func testImage(arch ec2types.ArchitectureValues, root string, gb int32) ec2types.Image {
	return ec2types.Image{ImageId: aws.String(testAMI), Name: aws.String("my-al2023-node"), State: ec2types.ImageStateAvailable,
		Architecture: arch, RootDeviceName: aws.String(root), RootDeviceType: ec2types.DeviceTypeEbs,
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{DeviceName: aws.String(root), Ebs: &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(gb)}}}}
}

func withAMI(id string) func(*clusterConfig) { return func(c *clusterConfig) { c.NodeAMIID = id } }

// latestData is the latest version of node group g's launch template.
func latestData(t *testing.T, f *clusterFake, name string) *ec2types.RequestLaunchTemplateData {
	t.Helper()
	for _, lt := range f.lts {
		if aws.ToString(lt.lt.LaunchTemplateName) == name {
			return lt.versions[len(lt.versions)-1]
		}
	}
	t.Fatalf("no launch template %s", name)
	return nil
}

// With NODE_AMI_ID every group's template names the AMI and carries the
// NodeConfig that joins the node with the group's labels and taints, the
// node groups take no AMI type, and a rerun writes nothing.
func TestCreateClusterCustomAMI(t *testing.T) {
	f := newClusterFake()
	f.images[testAMI] = testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
	if err := clusterTestRun(t, f, false, withAMI(testAMI)).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ name, lt, pool, taint string }{
		{"system", "demo-system-nodes", "system", "ate.dev/workloadType"},
		{"gvisor", "demo-gvisor-nodes", "gvisor", "ate.dev/sandboxClass"},
		{"microvm", "demo-microvm-nested-virt", "microvm", "ate.dev/sandboxClass"},
	} {
		if got := f.ngIn[g.name].AmiType; got != "" {
			t.Errorf("%s: AMI type %q with a custom AMI", g.name, got)
		}
		d := latestData(t, f, g.lt)
		if aws.ToString(d.ImageId) != testAMI {
			t.Errorf("%s: template image %q", g.name, aws.ToString(d.ImageId))
		}
		if d.MetadataOptions == nil || aws.ToInt32(d.MetadataOptions.HttpPutResponseHopLimit) != 1 {
			t.Errorf("%s: hop limit not 1", g.name)
		}
		_, nc := parseUserData(t, aws.ToString(d.UserData))
		spec := nc["spec"].(map[string]any)
		cl := spec["cluster"].(map[string]any)
		if cl["name"] != "demo" || cl["apiServerEndpoint"] != "https://FAKE.gr7.us-west-2.eks.amazonaws.com" ||
			cl["certificateAuthority"] != "RkFLRS1DQQ==" || cl["cidr"] != "10.100.0.0/16" {
			t.Errorf("%s: cluster %v", g.name, cl)
		}
		kl := spec["kubelet"].(map[string]any)
		flags := kl["flags"].([]any)
		if len(flags) != 1 || !strings.Contains(flags[0].(string), "ate.dev/pool="+g.pool) ||
			!strings.Contains(flags[0].(string), "eks.amazonaws.com/nodegroup="+g.name) ||
			!strings.Contains(flags[0].(string), "eks.amazonaws.com/nodegroup-image="+testAMI) {
			t.Errorf("%s: kubelet flags %v", g.name, flags)
		}
		taints, _ := kl["config"].(map[string]any)["registerWithTaints"].([]any)
		if len(taints) != 1 || taints[0].(map[string]any)["key"] != g.taint || taints[0].(map[string]any)["effect"] != "NoSchedule" {
			t.Errorf("%s: registerWithTaints %v", g.name, kl["config"])
		}
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false, withAMI(testAMI)).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("rerun writes %v", f.writes)
	}
}

// The default (no NODE_AMI_ID) sends what it always did: EKS's AMI type,
// and no image or user data in the templates.
func TestCreateClusterDefaultAMIUnchanged(t *testing.T) {
	f := newClusterFake()
	// A run that read an AMI would fail on this.
	f.deny["DescribeImages"] = "UnauthorizedOperation"
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, lt := range []string{"demo-system-nodes", "demo-gvisor-nodes", "demo-microvm-nested-virt"} {
		d := latestData(t, f, lt)
		if d.ImageId != nil || d.UserData != nil {
			t.Errorf("%s: image %v, user data %v", lt, d.ImageId, d.UserData)
		}
		if got := aws.ToString(d.BlockDeviceMappings[0].DeviceName); got != rootDevice {
			t.Errorf("%s: root device %s", lt, got)
		}
	}
	for _, g := range []string{"system", "gvisor", "microvm"} {
		if f.ngIn[g].AmiType != ekstypes.AMITypesAl2023X8664Standard {
			t.Errorf("%s: AMI type %s", g, f.ngIn[g].AmiType)
		}
	}
}

// The template's root volume is the AMI's own root device.
func TestCreateClusterCustomAMIRootDevice(t *testing.T) {
	f := newClusterFake()
	f.images[testAMI] = testImage(ec2types.ArchitectureValuesX8664, "/dev/sda1", 20)
	if err := clusterTestRun(t, f, false, withAMI(testAMI)).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(latestData(t, f, "demo-gvisor-nodes").BlockDeviceMappings[0].DeviceName); got != "/dev/sda1" {
		t.Errorf("root device %s", got)
	}
}

// --plan with a custom AMI before the cluster exists reads the AMI and
// writes nothing.
func TestCreateClusterCustomAMIPlan(t *testing.T) {
	f := newClusterFake()
	f.images[testAMI] = testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
	if err := clusterTestRun(t, f, true, withAMI(testAMI)).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("plan writes %v", f.writes)
	}
}

// Every AMI the run cannot use stops it in preflight, before any write; a
// denied read is never taken for a missing AMI.
func TestCreateClusterCustomAMIRefused(t *testing.T) {
	for name, c := range map[string]struct {
		id    string
		image *ec2types.Image
		deny  string
		want  string
	}{
		"not an AMI ID": {id: "ami-xyz", want: "not an AMI ID"},
		"missing":       {id: testAMI, want: "no such AMI"},
		"pending": {id: testAMI, image: func() *ec2types.Image {
			i := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
			i.State = ec2types.ImageStatePending
			return &i
		}(), want: "not available"},
		"wrong arch":   {id: testAMI, image: func() *ec2types.Image { i := testImage(ec2types.ArchitectureValuesArm64, "/dev/xvda", 20); return &i }(), want: "same architecture"},
		"root too big": {id: testAMI, image: func() *ec2types.Image { i := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 150); return &i }(), want: "root snapshot"},
		"instance store": {id: testAMI, image: func() *ec2types.Image {
			i := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
			i.RootDeviceType = ec2types.DeviceTypeInstanceStore
			return &i
		}(), want: "EBS root"},
		"windows": {id: testAMI, image: func() *ec2types.Image {
			i := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
			i.Platform = ec2types.PlatformValuesWindows
			return &i
		}(), want: "Windows"},
		"denied describe": {id: testAMI, image: func() *ec2types.Image { i := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20); return &i }(), deny: "UnauthorizedOperation", want: "describe AMI"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newClusterFake()
			if c.image != nil {
				f.images[testAMI] = *c.image
			}
			if c.deny != "" {
				f.deny["DescribeImages"] = c.deny
			}
			err := clusterTestRun(t, f, false, withAMI(c.id)).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if len(f.writes) != 0 {
				t.Errorf("writes before the refusal: %v", f.writes)
			}
		})
	}
}
