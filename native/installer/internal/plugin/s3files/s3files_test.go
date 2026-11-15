package s3files

import (
	"context"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3files"
	s3ftypes "github.com/aws/aws-sdk-go-v2/service/s3files/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
)

const (
	callerARN = "arn:aws:sts::111122223333:assumed-role/Admin/me"
	// The deployment's bucket for cluster demo in us-west-2.
	demoBucket = "arn:aws:s3:::demo-files-111122223333-us-west-2"
)

var demo = plugin.Options{Cluster: "demo", Region: "us-west-2", Profile: "p", Kubeconfig: "/kc"}

// fakeAWS is STS and S3 Files. List calls answer one page per call, by
// NextToken: fs is each bucket's file systems, page by page; aps is each
// file system's access points, page by page.
type fakeAWS struct {
	fs  map[string][][]s3ftypes.ListFileSystemsDescription
	aps map[string][][]string
}

func (f *fakeAWS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Arn: aws.String(callerARN), Account: aws.String("111122223333")}, nil
}

// pageOf returns pages[token] and the token of the next page, if any.
func pageOf[T any](pages [][]T, token *string) ([]T, *string) {
	i, _ := strconv.Atoi(aws.ToString(token))
	if i >= len(pages) {
		return nil, nil
	}
	if i+1 < len(pages) {
		return pages[i], aws.String(strconv.Itoa(i + 1))
	}
	return pages[i], nil
}

func (f *fakeAWS) ListFileSystems(_ context.Context, in *s3files.ListFileSystemsInput, _ ...func(*s3files.Options)) (*s3files.ListFileSystemsOutput, error) {
	got, next := pageOf(f.fs[aws.ToString(in.Bucket)], in.NextToken)
	return &s3files.ListFileSystemsOutput{FileSystems: got, NextToken: next}, nil
}

func (f *fakeAWS) ListAccessPoints(_ context.Context, in *s3files.ListAccessPointsInput, _ ...func(*s3files.Options)) (*s3files.ListAccessPointsOutput, error) {
	ids, next := pageOf(f.aps[aws.ToString(in.FileSystemId)], in.NextToken)
	out := &s3files.ListAccessPointsOutput{NextToken: next}
	for _, id := range ids {
		out.AccessPoints = append(out.AccessPoints, s3ftypes.ListAccessPointsDescription{AccessPointId: aws.String(id)})
	}
	return out, nil
}

// useAWS makes f the plugin's clients for the test.
func useAWS(t *testing.T, f *fakeAWS) {
	prev := clientsFor
	clientsFor = func(context.Context, plugin.Options) (stsAPI, s3filesAPI, error) { return f, f, nil }
	t.Cleanup(func() { clientsFor = prev })
}

func fsd(id string, status s3ftypes.LifeCycleState) s3ftypes.ListFileSystemsDescription {
	return s3ftypes.ListFileSystemsDescription{FileSystemId: aws.String(id), Bucket: aws.String(demoBucket), Status: status}
}

// Both ways: the deployment's bucket's file system that is not deleted is
// found, on any page; none, only a deleted one, or only another bucket's is
// the error that names `setup-aws plugin s3files`.
func TestFileSystemIDReadsTheDeploymentsBucket(t *testing.T) {
	for _, c := range []struct {
		name string
		fs   map[string][][]s3ftypes.ListFileSystemsDescription
		want string // "" is the error
	}{
		{"found", map[string][][]s3ftypes.ListFileSystemsDescription{demoBucket: {{fsd("fs-0abc", s3ftypes.LifeCycleStateAvailable)}}}, "fs-0abc"},
		{"deleted then live", map[string][][]s3ftypes.ListFileSystemsDescription{demoBucket: {{fsd("fs-0old", s3ftypes.LifeCycleStateDeleted), fsd("fs-0abc", s3ftypes.LifeCycleStateAvailable)}}}, "fs-0abc"},
		{"on the second page", map[string][][]s3ftypes.ListFileSystemsDescription{demoBucket: {{fsd("fs-0old", s3ftypes.LifeCycleStateDeleted)}, {fsd("fs-0def", s3ftypes.LifeCycleStateAvailable)}}}, "fs-0def"},
		{"none", map[string][][]s3ftypes.ListFileSystemsDescription{}, ""},
		{"deleted only", map[string][][]s3ftypes.ListFileSystemsDescription{demoBucket: {{fsd("fs-0old", s3ftypes.LifeCycleStateDeleted)}}}, ""},
		{"another bucket's", map[string][][]s3ftypes.ListFileSystemsDescription{"arn:aws:s3:::other-files-111122223333-us-west-2": {{fsd("fs-0abc", s3ftypes.LifeCycleStateAvailable)}}}, ""},
	} {
		useAWS(t, &fakeAWS{fs: c.fs})
		id, err := fileSystemID(context.Background(), demo)
		if c.want != "" {
			if err != nil || id != c.want {
				t.Errorf("%s: got %q, %v; want %s", c.name, id, err, c.want)
			}
			continue
		}
		const msg = "no S3 file system on bucket demo-files-<account>-us-west-2: run `setup-aws plugin s3files`"
		if err == nil || err.Error() != msg {
			t.Errorf("%s: got %q, %v; want the error %q", c.name, id, err, msg)
		}
	}
}

// Both ways: every page of access points is listed; a single page is not
// the whole list.
func TestAccessPointsListsEveryPage(t *testing.T) {
	useAWS(t, &fakeAWS{
		fs:  map[string][][]s3ftypes.ListFileSystemsDescription{demoBucket: {{fsd("fs-0abc", s3ftypes.LifeCycleStateAvailable)}}},
		aps: map[string][][]string{"fs-0abc": {{"fsap-1", "fsap-2"}, {"fsap-3"}}, "fs-0other": {{"fsap-9"}}},
	})
	ids, err := storage(demo).AccessPoints(context.Background())
	if want := []string{"fsap-1", "fsap-2", "fsap-3"}; err != nil || !slices.Equal(ids, want) {
		t.Errorf("got %q, %v; want %q", ids, err, want)
	}
	useAWS(t, &fakeAWS{fs: map[string][][]s3ftypes.ListFileSystemsDescription{}})
	if _, err := storage(demo).AccessPoints(context.Background()); err == nil || !strings.Contains(err.Error(), "run `setup-aws plugin s3files`") {
		t.Errorf("no file system: %v", err)
	}
}

// The lookups never start an `aws` child: an AWS CLI older than S3 Files
// (aws-cli 2.32.11) exits 252 on `aws s3files`. PATH has no aws at all,
// and every child is recorded.
func TestLookupsRunNoAWSCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var children []string
	prev := plugin.Exec
	plugin.Exec = func(ctx context.Context, w io.Writer, c plugin.Cmd) error {
		children = append(children, strings.Join(c.Argv, " "))
		return prev(ctx, w, c)
	}
	t.Cleanup(func() { plugin.Exec = prev })
	useAWS(t, &fakeAWS{
		fs:  map[string][][]s3ftypes.ListFileSystemsDescription{demoBucket: {{fsd("fs-0abc", s3ftypes.LifeCycleStateAvailable)}}},
		aps: map[string][][]string{"fs-0abc": {{"fsap-1"}}},
	})
	if id, err := fileSystemID(context.Background(), demo); err != nil || id != "fs-0abc" {
		t.Errorf("fileSystemID: %q, %v", id, err)
	}
	if ids, err := storage(demo).AccessPoints(context.Background()); err != nil || !slices.Equal(ids, []string{"fsap-1"}) {
		t.Errorf("access points: %q, %v", ids, err)
	}
	if len(children) > 0 {
		t.Errorf("the lookups started children: %q", children)
	}
}

func TestStorageClassIsS3FilesAccessPoints(t *testing.T) {
	sc, err := efs.Render(efs.StorageTmpl, efs.StorageArgs{Name: StorageClass, Mode: "s3files-ap", FileSystem: "fs-0abc", UID: efs.UID, GID: efs.GID, BasePath: "/ate"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(sc)
	for _, want := range []string{"name: s3files-ate", "provisioningMode: s3files-ap", "fileSystemId: fs-0abc", "provisioner: efs.csi.aws.com", "reclaimPolicy: Delete"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	// Anchored: the template's own comment names the kind.
	if regexp.MustCompile(`(?m)^kind: CSIDriverConfig$`).MatchString(s) {
		t.Error("s3files applies the efs plugin's CSIDriverConfig again")
	}
}
