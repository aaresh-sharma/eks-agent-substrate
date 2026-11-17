package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// Each node group gets its own launch template with the EC2 settings that
// matter for isolation: IMDSv2 only with a hop limit of 1, so a pod in its
// own network namespace cannot get an IMDS token (and with it the node
// role's credentials); an encrypted gp3 root volume; and, on microvm, nested
// virtualization, which gives a regular instance /dev/kvm for Kata. Without
// a template EKS makes one with hop limit 2.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html
//	https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-options.html
//	https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
type ltRef struct {
	id      string
	version int64
}

// launchTemplateName is the node group's launch template:
// <cluster>-microvm-nested-virt for microvm, <cluster>-<group>-nodes for the
// system and gvisor groups.
func (r *clusterRun) launchTemplateName(g nodeGroupSpec) string {
	if g.NestedVirt {
		return r.e.names.Cluster + "-microvm-nested-virt"
	}
	return r.e.names.Cluster + "-" + g.Name + "-nodes"
}

// rootDevice is the root volume of the Amazon Linux 2023 EKS AMIs.
const rootDevice = "/dev/xvda"

// groupImage is the AMI group g's template names: NODE_AMI_ID, or for a
// self-managed group the recommended EKS optimized AL2023 AMI of its
// architecture; nil when EKS picks it (a managed group without NODE_AMI_ID).
func (r *clusterRun) groupImage(g nodeGroupSpec) *nodeImage {
	if r.image != nil {
		return r.image
	}
	if r.selfManaged() {
		return r.archImages[archOf(r.amiTypes[g.InstanceType])]
	}
	return nil
}

// groupRootDevice is the root device of group g's AMI: the named AMI's
// own, else the EKS AL2023 AMIs'.
func (r *clusterRun) groupRootDevice(g nodeGroupSpec) string {
	if im := r.groupImage(g); im != nil {
		return im.RootDevice
	}
	return rootDevice
}

func (r *clusterRun) launchTemplateData(g nodeGroupSpec) (*ec2types.RequestLaunchTemplateData, error) {
	d := &ec2types.RequestLaunchTemplateData{
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{
			HttpTokens:              ec2types.LaunchTemplateHttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            ec2types.LaunchTemplateInstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMappingRequest{{
			DeviceName: aws.String(r.groupRootDevice(g)),
			Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{VolumeSize: aws.Int32(g.VolumeGB), VolumeType: ec2types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true)},
		}},
	}
	if g.NestedVirt {
		d.CpuOptions = &ec2types.LaunchTemplateCpuOptionsRequest{NestedVirtualization: ec2types.NestedVirtualizationSpecificationEnabled}
	}
	// With a custom AMI, EKS adds no bootstrap: the node joins from the
	// NodeConfig in the template's user data. A self-managed node always
	// does, and also names its instance profile and security group, which
	// EKS sets for a managed group's nodes.
	im := r.groupImage(g)
	if r.selfManaged() && im == nil {
		return nil, fmt.Errorf("internal: node group %s has no AMI", g.Name)
	}
	if im != nil {
		ud, err := r.groupUserData(g, im)
		if err != nil {
			return nil, err
		}
		d.ImageId, d.UserData = aws.String(im.ID), aws.String(ud)
	}
	if r.selfManaged() {
		sg, err := r.clusterSecurityGroup()
		if err != nil {
			return nil, fmt.Errorf("node group %s: %w", g.Name, err)
		}
		d.IamInstanceProfile = &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{Name: aws.String(r.instanceProfileName())}
		d.SecurityGroupIds = []string{sg}
		// An Auto Scaling group without a mixed instances policy takes its
		// instance type from the template; a managed node group takes it
		// in CreateNodegroup instead.
		d.InstanceType = ec2types.InstanceType(g.InstanceType)
	}
	// The node group's instances, their volumes and network interfaces get
	// the deployment's tags at launch.
	name := r.e.names.Cluster + "-" + g.Name
	for _, t := range []ec2types.ResourceType{ec2types.ResourceTypeInstance, ec2types.ResourceTypeVolume, ec2types.ResourceTypeNetworkInterface} {
		d.TagSpecifications = append(d.TagSpecifications, ec2types.LaunchTemplateTagSpecificationRequest{
			ResourceType: t, Tags: r.ec2Tags(map[string]string{"Name": name})})
	}
	return d, nil
}

// groupUserData is the NodeConfig of a node of group g from AMI im: the
// cluster's join data, and the group's labels and taints, plus, on a managed
// group, the labels EKS gives the nodes of a managed node group from its own
// AMI (EKS applies a group's API labels to a node from a custom AMI only
// after it joins). Under --plan before the cluster exists, the join data is
// a placeholder: no template is written then.
func (r *clusterRun) groupUserData(g nodeGroupSpec, im *nodeImage) (string, error) {
	var j nodeJoin
	var err error
	switch {
	case r.cluster != nil && aws.ToString(r.cluster.Endpoint) != "":
		j, err = nodeJoinFor(r.cluster)
	case r.plan():
		j = nodeJoin{Cluster: r.e.names.Cluster, Endpoint: "https://planned", CA: "planned", ServiceCIDR: "planned"}
	default:
		err = fmt.Errorf("cluster %s does not exist yet", r.e.names.Cluster)
	}
	if err != nil {
		return "", fmt.Errorf("node group %s: %w", g.Name, err)
	}
	labels := map[string]string{}
	if !r.selfManaged() {
		labels = map[string]string{
			"eks.amazonaws.com/nodegroup":       g.Name,
			"eks.amazonaws.com/nodegroup-image": im.ID,
			"eks.amazonaws.com/capacityType":    string(ekstypes.CapacityTypesOnDemand),
		}
	}
	maps.Copy(labels, g.Labels)
	return nodeUserData(j, labels, g.Taints)
}

// ltSettings are the settings of a template version that matter for
// isolation and for how the node joins, as one comparable value.
type ltSettings struct {
	NestedVirt, Tokens, Endpoint, VolumeType string
	HopLimit, VolumeSize                     int32
	Encrypted, DeleteOnTermination           bool
	RootDevice, ImageID, UserData            string
	// A self-managed node's instance profile and security groups (sorted,
	// comma-separated).
	InstanceProfile, SecurityGroups string
	InstanceType                    string
}

func requestSettings(d *ec2types.RequestLaunchTemplateData) ltSettings {
	s := ltSettings{ImageID: aws.ToString(d.ImageId), UserData: aws.ToString(d.UserData), InstanceType: string(d.InstanceType),
		SecurityGroups: strings.Join(sortedCopy(d.SecurityGroupIds), ",")}
	if d.IamInstanceProfile != nil {
		s.InstanceProfile = aws.ToString(d.IamInstanceProfile.Name)
	}
	if d.CpuOptions != nil {
		s.NestedVirt = string(d.CpuOptions.NestedVirtualization)
	}
	if m := d.MetadataOptions; m != nil {
		s.Tokens, s.Endpoint, s.HopLimit = string(m.HttpTokens), string(m.HttpEndpoint), aws.ToInt32(m.HttpPutResponseHopLimit)
	}
	for _, b := range d.BlockDeviceMappings {
		if b.Ebs != nil {
			s.RootDevice = aws.ToString(b.DeviceName)
			s.VolumeType, s.VolumeSize = string(b.Ebs.VolumeType), aws.ToInt32(b.Ebs.VolumeSize)
			s.Encrypted, s.DeleteOnTermination = aws.ToBool(b.Ebs.Encrypted), aws.ToBool(b.Ebs.DeleteOnTermination)
		}
	}
	return s
}

func responseSettings(d *ec2types.ResponseLaunchTemplateData) ltSettings {
	s := ltSettings{}
	if d == nil {
		return s
	}
	s.ImageID, s.UserData, s.InstanceType = aws.ToString(d.ImageId), aws.ToString(d.UserData), string(d.InstanceType)
	s.SecurityGroups = strings.Join(sortedCopy(d.SecurityGroupIds), ",")
	if d.IamInstanceProfile != nil {
		s.InstanceProfile = aws.ToString(d.IamInstanceProfile.Name)
	}
	if d.CpuOptions != nil {
		s.NestedVirt = string(d.CpuOptions.NestedVirtualization)
	}
	if m := d.MetadataOptions; m != nil {
		s.Tokens, s.Endpoint, s.HopLimit = string(m.HttpTokens), string(m.HttpEndpoint), aws.ToInt32(m.HttpPutResponseHopLimit)
	}
	for _, b := range d.BlockDeviceMappings {
		if b.Ebs != nil {
			s.RootDevice = aws.ToString(b.DeviceName)
			s.VolumeType, s.VolumeSize = string(b.Ebs.VolumeType), aws.ToInt32(b.Ebs.VolumeSize)
			s.Encrypted, s.DeleteOnTermination = aws.ToBool(b.Ebs.Encrypted), aws.ToBool(b.Ebs.DeleteOnTermination)
		}
	}
	return s
}

// ensureLaunchTemplates makes the template of every node group still to
// create. An existing node group keeps the template version it runs: moving
// it to a new version replaces its nodes, which is a decision for a person.
func (r *clusterRun) ensureLaunchTemplates(ctx context.Context) error {
	for _, g := range r.groups() {
		if r.groupExists(g.Name) {
			slog.Info("node group exists; its launch template is left as it is", "nodegroup", g.Name)
			continue
		}
		ref, err := r.ensureLaunchTemplate(ctx, g)
		if err != nil {
			return fmt.Errorf("launch template %s: %w", r.launchTemplateName(g), err)
		}
		r.templates[g.Name] = ref
	}
	return nil
}

// ensureLaunchTemplate creates the template, or reuses it when it carries
// this deployment's owner tag: as is when its latest version has the wanted
// settings, else with a new version that has them. A template without the
// owner tag is never used or changed.
func (r *clusterRun) ensureLaunchTemplate(ctx context.Context, g nodeGroupSpec) (ltRef, error) {
	name := r.launchTemplateName(g)
	data, err := r.launchTemplateData(g)
	if err != nil {
		return ltRef{}, err
	}
	out, err := r.c.EC2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{Filters: []ec2types.Filter{filter("launch-template-name", name)}})
	if err != nil {
		return ltRef{}, fmt.Errorf("describe: %w", err)
	}
	if len(out.LaunchTemplates) == 0 {
		ref := ltRef{id: plannedPrefix + name, version: 1}
		err := r.create("launch template "+name, func() error {
			o, err := r.c.EC2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{LaunchTemplateName: aws.String(name),
				LaunchTemplateData: data, TagSpecifications: r.tagSpec(ec2types.ResourceTypeLaunchTemplate, name)})
			if err != nil {
				return err
			}
			ref = ltRef{id: aws.ToString(o.LaunchTemplate.LaunchTemplateId), version: aws.ToInt64(o.LaunchTemplate.LatestVersionNumber)}
			return nil
		})
		return ref, err
	}
	lt := out.LaunchTemplates[0]
	ref := ltRef{id: aws.ToString(lt.LaunchTemplateId), version: aws.ToInt64(lt.LatestVersionNumber)}
	if owner := ec2TagMap(lt.Tags)[OwnerTagKey]; owner != r.e.names.OwnerValue {
		return ref, fmt.Errorf("%s exists but is not owned by %s (%s=%q); delete or rename it, or pick another CLUSTER_NAME",
			ref.id, r.e.names.OwnerValue, OwnerTagKey, owner)
	}
	v, err := r.c.EC2.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(ref.id), Versions: []string{strconv.FormatInt(ref.version, 10)}})
	if err != nil {
		return ref, fmt.Errorf("describe version %d of %s: %w", ref.version, ref.id, err)
	}
	if len(v.LaunchTemplateVersions) == 1 && responseSettings(v.LaunchTemplateVersions[0].LaunchTemplateData) == requestSettings(data) {
		slog.Info("reusing launch template", "template", name, "id", ref.id, "version", ref.version)
		return ref, nil
	}
	err = r.change(fmt.Sprintf("add a version with the wanted settings to launch template %s (%s)", name, ref.id), func() error {
		o, err := r.c.EC2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
			LaunchTemplateId: aws.String(ref.id), LaunchTemplateData: data})
		if err != nil {
			return err
		}
		ref.version = aws.ToInt64(o.LaunchTemplateVersion.VersionNumber)
		return nil
	})
	return ref, err
}
