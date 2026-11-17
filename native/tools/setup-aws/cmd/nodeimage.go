package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// A custom AMI (NODE_AMI_ID) replaces the Amazon EKS optimized Amazon Linux
// 2023 AMI that EKS picks for a managed node group by its AMI type. The AMI
// must join the cluster with nodeadm, as AMIs built from the EKS optimized
// AL2023 AMI or with awslabs/amazon-eks-ami do: the launch template gives
// it a NodeConfig (nodeUserData). It is the customer's: no owner tag, so
// teardown never deletes it.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html#launch-template-custom-ami
//	https://docs.aws.amazon.com/eks/latest/userguide/eks-optimized-ami.html
type nodeImage struct {
	ID         string
	Name       string
	Arch       ec2types.ArchitectureValues
	RootDevice string
	// RootGB is the size of the root volume's snapshot, the smallest root
	// volume the AMI launches with.
	RootGB int32
	// ENA reports that the AMI has ENA support, which current instance
	// types (the m8i default) require.
	ENA bool
}

// amiIDRE is an EC2 image ID.
var amiIDRE = regexp.MustCompile(`^ami-([0-9a-f]{8}|[0-9a-f]{17})$`)

// archOf is the image architecture of an EKS AMI type.
func archOf(t ekstypes.AMITypes) ec2types.ArchitectureValues {
	if t == ekstypes.AMITypesAl2023Arm64Standard {
		return ec2types.ArchitectureValuesArm64
	}
	return ec2types.ArchitectureValuesX8664
}

// describeImage reads one AMI. An AMI that does not exist, or that this
// account may not launch, is an error with the way out; any other failed
// read stops the run too (it is never taken as "no such AMI").
func describeImage(ctx context.Context, c clusterEC2API, id, setting string) (*nodeImage, error) {
	if !amiIDRE.MatchString(id) {
		return nil, fmt.Errorf("%s=%s is not an AMI ID (ami- and 8 or 17 hexadecimal digits)", setting, id)
	}
	out, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
	switch {
	case hasCode(err, "InvalidAMIID.NotFound"), hasCode(err, "InvalidAMIID.Unavailable"), err == nil && len(out.Images) == 0:
		return nil, fmt.Errorf("%s=%s: no such AMI in this Region, or this account may not launch it; copy it to this Region or share it with this account", setting, id)
	case hasCode(err, "InvalidAMIID.Malformed"):
		return nil, fmt.Errorf("%s=%s is not an AMI ID", setting, id)
	case err != nil:
		return nil, fmt.Errorf("describe AMI %s (%s): %w", id, setting, err)
	}
	im := out.Images[0]
	if im.State != ec2types.ImageStateAvailable {
		return nil, fmt.Errorf("%s=%s is %s, not available", setting, id, im.State)
	}
	if im.Platform == ec2types.PlatformValuesWindows {
		return nil, fmt.Errorf("%s=%s is a Windows AMI; a node AMI here joins with nodeadm (Amazon Linux 2023)", setting, id)
	}
	if im.VirtualizationType != ec2types.VirtualizationTypeHvm {
		return nil, fmt.Errorf("%s=%s is a %s AMI; node instance types run HVM AMIs only", setting, id, im.VirtualizationType)
	}
	n := &nodeImage{ID: id, Name: aws.ToString(im.Name), Arch: im.Architecture, RootDevice: aws.ToString(im.RootDeviceName),
		ENA: aws.ToBool(im.EnaSupport)}
	if n.RootDevice == "" {
		return nil, fmt.Errorf("%s=%s has no root device", setting, id)
	}
	ebs := false
	for _, b := range im.BlockDeviceMappings {
		if aws.ToString(b.DeviceName) == n.RootDevice && b.Ebs != nil {
			ebs, n.RootGB = true, aws.ToInt32(b.Ebs.VolumeSize)
		}
	}
	if !ebs || im.RootDeviceType != ec2types.DeviceTypeEbs {
		return nil, fmt.Errorf("%s=%s has no EBS root volume; a node AMI here is EBS-backed", setting, id)
	}
	return n, nil
}

// checkImage reads NODE_AMI_ID, when it is set, for the groups to make: its
// architecture must be the one each group's instance type runs, and its root
// snapshot must fit the group's root volume. checkInstanceTypes runs first.
func (r *clusterRun) checkImage(ctx context.Context) error {
	if r.cfg.NodeAMIID == "" || len(r.groups()) == 0 {
		return nil
	}
	// The NodeConfig carries an IPv4 service CIDR; a new cluster is IPv4.
	if r.cluster != nil && r.cluster.KubernetesNetworkConfig != nil && r.cluster.KubernetesNetworkConfig.IpFamily == ekstypes.IpFamilyIpv6 {
		return fmt.Errorf("cluster %s is an IPv6 cluster; NODE_AMI_ID supports IPv4 clusters only", r.e.names.Cluster)
	}
	im, err := describeImage(ctx, r.c.EC2, r.cfg.NodeAMIID, "NODE_AMI_ID")
	if err != nil {
		return err
	}
	for _, g := range r.groups() {
		if r.enaRequired[g.InstanceType] && !im.ENA {
			return fmt.Errorf("NODE_AMI_ID=%s has no ENA support, which the %s node group's %s requires", im.ID, g.Name, g.InstanceType)
		}
		if want := archOf(r.amiTypes[g.InstanceType]); im.Arch != want {
			return fmt.Errorf("NODE_AMI_ID=%s is %s, but the %s node group's %s runs %s; pick an AMI or an instance type of the same architecture",
				im.ID, im.Arch, g.Name, g.InstanceType, want)
		}
		if im.RootGB > g.VolumeGB {
			return fmt.Errorf("NODE_AMI_ID=%s has a %d GB root snapshot, more than the %s node group's %d GB root volume", im.ID, im.RootGB, g.Name, g.VolumeGB)
		}
	}
	r.image = im
	slog.Info("custom node AMI", "ami", im.ID, "name", im.Name, "arch", string(im.Arch), "rootDevice", im.RootDevice)
	return nil
}
