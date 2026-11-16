package s3files

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3files"
	s3ftypes "github.com/aws/aws-sdk-go-v2/service/s3files/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/awsload"
)

const (
	ID           = "s3files"
	StorageClass = "s3files-ate"
	Template     = "counter-s3files"
)

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Amazon S3 Files volumes for actors",
		Summary: "Each actor gets its own S3 Files access point as its data volume, its files synced to a private, versioned S3 bucket " +
			"(ExternalVolumes preview). Uses the efs plugin's CSI driver. Adds counter-s3files to the counter demo.",
		Order:    31,
		Minutes:  10,
		Requires: []string{efs.ID},
		SetupAWS: true,
		// Its templates and Verify's actors are the counter demo's.
		NeedsDemo: true,
		Needs:     plugin.Needs{Preview: []string{"ExternalVolumes"}},
		// S3 Files bills stored data and requests, not hours.
		HourlyUSD: 0.01,
		Deploy:    deploy,
		Verify:    verify,
		Remove:    remove,
	})
}

// stsAPI and s3filesAPI are the calls the plugin's lookups make.
type stsAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type s3filesAPI interface {
	s3files.ListFileSystemsAPIClient
	s3files.ListAccessPointsAPIClient
}

// clientsFor is o's STS and S3 Files clients, loaded through awsload like
// every step's. The lookups use the AWS SDK, not the AWS CLI: a CLI older
// than S3 Files (aws-cli 2.32.11) has no s3files command. Tests replace it.
var clientsFor = func(ctx context.Context, o plugin.Options) (stsAPI, s3filesAPI, error) {
	cfg, err := awsload.Config(ctx, o.Region, o.Profile)
	if err != nil {
		return nil, nil, err
	}
	return sts.NewFromConfig(cfg), s3files.NewFromConfig(cfg), nil
}

// fileSystemID is the S3 file system on this deployment's bucket
// <cluster>-files-<account>-<region> (`setup-aws plugin s3files` made both):
// the first one listed that is not deleted.
func fileSystemID(ctx context.Context, o plugin.Options) (string, error) {
	st, c, err := clientsFor(ctx, o)
	if err != nil {
		return "", err
	}
	me, err := st.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("AWS credentials: %w", err)
	}
	p := strings.Split(aws.ToString(me.Arn), ":")
	if len(p) < 5 {
		return "", fmt.Errorf("unexpected caller identity")
	}
	bucket := fmt.Sprintf("arn:%s:s3:::%s-files-%s-%s", p[1], o.Cluster, p[4], o.Region)
	id := ""
	pages := s3files.NewListFileSystemsPaginator(c, &s3files.ListFileSystemsInput{Bucket: aws.String(bucket)})
	for id == "" && pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("list S3 file systems: %w", err)
		}
		for _, f := range page.FileSystems {
			if f.Status != s3ftypes.LifeCycleStateDeleted {
				id = aws.ToString(f.FileSystemId)
				break
			}
		}
	}
	if !strings.HasPrefix(id, "fs-") {
		return "", fmt.Errorf("no S3 file system on bucket %s-files-<account>-%s: run `setup-aws plugin s3files`", o.Cluster, o.Region)
	}
	return id, nil
}

// accessPointIDs lists the access points of o's S3 file system.
func accessPointIDs(ctx context.Context, o plugin.Options) ([]string, error) {
	fs, err := fileSystemID(ctx, o)
	if err != nil {
		return nil, err
	}
	_, c, err := clientsFor(ctx, o)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	pages := s3files.NewListAccessPointsPaginator(c, &s3files.ListAccessPointsInput{FileSystemId: aws.String(fs)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list access points of %s: %w", fs, err)
		}
		for _, a := range page.AccessPoints {
			ids = append(ids, aws.ToString(a.AccessPointId))
		}
	}
	return ids, nil
}

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	// The node plugin mounts S3 Files with its own Pod Identity role, which
	// `setup-aws plugin s3files` associated after the add-on started.
	if err := efs.EnsurePodIdentity(ctx, w, o, "kube-system", "daemonset/efs-csi-node"); err != nil {
		return err
	}
	fs, err := fileSystemID(ctx, o)
	if err != nil {
		return err
	}
	sc, err := efs.Render(efs.StorageTmpl, efs.StorageArgs{Name: StorageClass, Mode: "s3files-ap", FileSystem: fs, UID: efs.UID, GID: efs.GID, BasePath: "/ate"})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Applying StorageClass %s (S3 file system %s)\n", StorageClass, fs)
	if err := efs.ApplyStorageClass(ctx, w, o, StorageClass, fs, efs.UID, sc); err != nil {
		return err
	}
	k, err := efs.BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	return efs.CreateTemplates(ctx, w, k, Template, StorageClass)
}

func storage(o plugin.Options) efs.Storage {
	return efs.Storage{Name: ID, Template: Template, AccessPoints: func(ctx context.Context) ([]string, error) {
		return accessPointIDs(ctx, o)
	}}
}

// verify is the efs plugin's round trip on S3 Files, on every counter class:
// about 2 minutes per class.
func verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	k, err := efs.BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	for _, c := range efs.Classes {
		if err := efs.RoundTrip(ctx, w, o, k, storage(o), c, false); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}

// remove deletes the counter-s3files actors (their access points go with
// them) and templates, and the StorageClass. `setup-aws plugin s3files
// --delete` then deletes the file system, the bucket and the roles.
func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if k, err := efs.BuildAte(ctx, w, o); err != nil {
		fmt.Fprintf(w, "WARNING: %v; leaving the %s actors to the file system's deletion\n", err, Template)
	} else if err := efs.DeleteTemplates(ctx, w, k, Template); err != nil {
		return err
	}
	return efs.DeleteStorageClass(ctx, w, o, StorageClass)
}
