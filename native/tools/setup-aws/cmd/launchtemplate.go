package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strconv"

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

// groupRootDevice is the root device of the groups' AMI: a custom AMI's
// own, else the EKS AL2023 AMIs'.
func (r *clusterRun) groupRootDevice() string {
	if r.image != nil {
		return r.image.RootDevice
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
			DeviceName: aws.String(r.groupRootDevice()),
			Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{VolumeSize: aws.Int32(g.VolumeGB), VolumeType: ec2types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true)},
		}},
	}
	if g.NestedVirt {
		d.CpuOptions = &ec2types.LaunchTemplateCpuOptionsRequest{NestedVirtualization: ec2types.NestedVirtualizationSpecificationEnabled}
	}
	// With a custom AMI, EKS adds no bootstrap: the node joins from the
	// NodeConfig in the template's user data.
	if r.image != nil {
		ud, err := r.groupUserData(g)
		if err != nil {
			return nil, err
		}
		d.ImageId, d.UserData = aws.String(r.image.ID), aws.String(ud)
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

// groupUserData is the NodeConfig of a node of group g from a custom AMI:
// the cluster's join data, and the group's labels and taints, plus the
// labels EKS gives the nodes of a managed node group from its own AMI (EKS
// applies a group's API labels to a node from a custom AMI only after it
// joins). Under --plan before the cluster exists, the join data is a
// placeholder: no template is written then.
func (r *clusterRun) groupUserData(g nodeGroupSpec) (string, error) {
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
	labels := map[string]string{
		"eks.amazonaws.com/nodegroup":       g.Name,
		"eks.amazonaws.com/nodegroup-image": r.image.ID,
		"eks.amazonaws.com/capacityType":    string(ekstypes.CapacityTypesOnDemand),
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
}

func requestSettings(d *ec2types.RequestLaunchTemplateData) ltSettings {
	s := ltSettings{ImageID: aws.ToString(d.ImageId), UserData: aws.ToString(d.UserData)}
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
	s.ImageID, s.UserData = aws.ToString(d.ImageId), aws.ToString(d.UserData)
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
		if r.nodegroups[g.Name] != nil {
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
