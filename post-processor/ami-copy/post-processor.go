//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type Config,Target

package ami_copy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/sourcegraph/conc/pool"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sts"

	"github.com/hashicorp/packer-plugin-amazon/builder/chroot"
	"github.com/hashicorp/packer-plugin-amazon/builder/ebs"
	"github.com/hashicorp/packer-plugin-amazon/builder/ebssurrogate"
	"github.com/hashicorp/packer-plugin-amazon/builder/ebsvolume"
	"github.com/hashicorp/packer-plugin-amazon/builder/instance"

	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"

	"github.com/bdwyertech/packer-plugin-aws-ami/helpers"

	awscommon "github.com/hashicorp/packer-plugin-amazon/builder/common"
)

// BuilderId is the ID of this post processor.
// nolint: golint
const BuilderId = "packer.post-processor.ami-copy"

// Config is the post-processor configuration with interpolation supported.
// See https://www.packer.io/docs/builders/amazon.html for details.
type Config struct {
	common.PackerConfig    `mapstructure:",squash"`
	awscommon.AccessConfig `mapstructure:",squash"`
	awscommon.AMIConfig    `mapstructure:",squash"`

	// Variables specific to this post-processor
	RoleName        string `mapstructure:"role_name"`
	CopyConcurrency int    `mapstructure:"copy_concurrency"`
	EnsureAvailable bool   `mapstructure:"ensure_available"`
	KeepArtifact    string `mapstructure:"keep_artifact"`
	ManifestOutput  string `mapstructure:"manifest_output"`
	TagsOnly        bool   `mapstructure:"tags_only"`

	Targets []Target `mapstructure:"targets"`

	ctx interpolate.Context
}

type Target struct {
	awscommon.AccessConfig `mapstructure:",squash"`
	Name                   string `mapstructure:"name"`
}

// PostProcessor implements Packer's PostProcessor interface.
type PostProcessor struct {
	config Config
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec {
	return p.config.FlatMapstructure().HCL2Spec()
}

// Configure interpolates and validates requisite vars for the PostProcessor.
func (p *PostProcessor) Configure(raws ...interface{}) error {
	p.config.ctx.Funcs = awscommon.TemplateFuncs

	if err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         BuilderId,
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{},
		},
	}, raws...); err != nil {
		return err
	}

	if len(p.config.AMIUsers) == 0 && len(p.config.Targets) == 0 {
		return errors.New("ami_users or targets must be set")
	}

	if len(p.config.KeepArtifact) == 0 {
		p.config.KeepArtifact = "true"
	}

	return nil
}

// PostProcess will copy the source AMI to each of the target accounts as
// designated by the mandatory `ami_users` variable. It will optionally
// encrypt the copied AMIs (`encrypt_boot`) with `kms_key_id` if set, or the
// default EBS KMS key if unset. Tags will be copied with the image.
//
// Copies are executed concurrently. This concurrency is unlimited unless
// controller by `copy_concurrency`.
func (p *PostProcessor) PostProcess(
	ctx context.Context, ui packer.Ui, artifact packer.Artifact) (packer.Artifact, bool, bool, error) {

	keepArtifactBool, err := strconv.ParseBool(p.config.KeepArtifact)
	if err != nil {
		return artifact, keepArtifactBool, false, err
	}

	// Ensure we're being called from a supported builder
	switch artifact.BuilderId() {
	case ebs.BuilderId,
		ebssurrogate.BuilderId,
		ebsvolume.BuilderId,
		chroot.BuilderId,
		instance.BuilderId:
		break
	default:
		return artifact, keepArtifactBool, false,
			fmt.Errorf("Unexpected artifact type: %s\nCan only export from Amazon builders",
				artifact.BuilderId())
	}

	if awsArtifact, ok := artifact.(*awscommon.Artifact); ok {
		ui.Sayf("Passed Build Artifacts: %v", awsArtifact.Amis)
		s := awsArtifact.Session
		resp, err := sts.New(s).GetCallerIdentity(&sts.GetCallerIdentityInput{})
		if err == nil {
			ui.Say("Prior Session: " + resp.String())
		}
	}

	// Current AWS session
	currSession, err := p.config.Session()
	if err != nil {
		return artifact, keepArtifactBool, false, err
	}

	// Copy futures
	var (
		amis   = amisFromArtifactID(artifact.Id())
		users  = p.config.AMIUsers
		copies []AmiCopy
	)
	for _, ami := range amis {
		var source *ec2.Image
		if source, err = helpers.LocateSingleAMI(
			ami.id,
			ec2.New(currSession, aws.NewConfig().WithRegion(ami.region)),
		); err != nil || source == nil {
			return artifact, keepArtifactBool, false, err
		}

		var conns []*ec2.EC2
		for _, tgt := range p.config.Targets {
			session, err := tgt.Session()
			if err != nil {
				ui.Error(err.Error())
				continue
			}
			conns = append(conns, ec2.New(session, &aws.Config{Region: aws.String(ami.region)}))
		}

		for _, user := range users {
			if p.config.RoleName != "" {
				var (
					role = fmt.Sprintf("arn:aws:iam::%s:role/%s", user, p.config.RoleName)
					sess = currSession.Copy(&aws.Config{Region: aws.String(ami.region)})
				)
				conns = append(conns, ec2.New(sess, &aws.Config{
					Credentials: stscreds.NewCredentials(sess, role),
				}))
			} else {
				conns = append(conns, ec2.New(currSession.Copy(&aws.Config{Region: aws.String(ami.region)})))
			}
		}

		var sayOnce sync.Once
		for _, conn := range conns {
			var name, description string
			if source.Name != nil {
				name = *source.Name
			}
			if source.Description != nil {
				description = *source.Description
			}

			sayOnce.Do(func() {
				ui.Sayf(fmt.Sprintf("Source Tags: %v", source.Tags))
			})

			amiCopy := &AmiCopyImpl{
				EC2:             conn,
				SourceImage:     source,
				EnsureAvailable: p.config.EnsureAvailable,
				TagsOnly:        p.config.TagsOnly,
				Tags:            p.config.AMITags,
			}
			amiCopy.SetTargetAccountID("self")
			amiCopy.SetInput(&ec2.CopyImageInput{
				Name:          aws.String(name),
				Description:   aws.String(description),
				SourceImageId: aws.String(ami.id),
				SourceRegion:  aws.String(ami.region),
				KmsKeyId:      aws.String(p.config.AMIKmsKeyId),
				Encrypted:     aws.Bool(p.config.AMIEncryptBootVolume.True()),
			})

			copies = append(copies, amiCopy)
		}
	}

	copyErrs := copyAMIs(ctx, copies, ui, p.config.ManifestOutput, p.config.CopyConcurrency)
	if copyErrCount := len(copyErrs.Errors); copyErrCount > 0 {
		return artifact, true, false, fmt.Errorf(
			"%d/%d AMI copies failed, manual reconciliation may be required", copyErrCount, len(copies))
	}

	return artifact, keepArtifactBool, false, nil
}

func copyAMIs(ctx context.Context, copies []AmiCopy, ui packer.Ui, manifestOutput string, concurrencyCount int) (errs packer.MultiError) {
	// Copy execution loop
	var (
		copyCount    = len(copies)
		amiManifests = make(chan *AmiManifest, copyCount)
	)
	if concurrencyCount == 0 { // Unlimited
		concurrencyCount = copyCount
	}
	p := pool.New().WithContext(ctx).WithMaxGoroutines(concurrencyCount)
	for _, c := range copies {
		p.Go(func(_ context.Context) error {
			input := c.Input()
			ui.Say(
				fmt.Sprintf(
					"[%s] Copying %s to account %s (encrypted: %t)",
					*input.SourceRegion,
					*input.SourceImageId,
					c.TargetAccountID(),
					*input.Encrypted,
				),
			)
			if err := c.Copy(&ui); err != nil {
				ui.Error(err.Error())
				packer.MultiErrorAppend(&errs, err)
				return err
			}
			output := c.Output()
			manifest := &AmiManifest{
				AccountID: c.TargetAccountID(),
				Region:    *input.SourceRegion,
				ImageID:   *output.ImageId,
			}
			amiManifests <- manifest

			ui.Say(
				fmt.Sprintf(
					"[%s] Finished copying %s to %s (copied id: %s)",
					*input.SourceRegion,
					*input.SourceImageId,
					c.TargetAccountID(),
					*output.ImageId,
				),
			)
			return nil
		})
	}
	p.Wait()

	if manifestOutput != "" {
		manifests := []*AmiManifest{}
	LOOP:
		for {
			select {
			case m := <-amiManifests:
				manifests = append(manifests, m)
			default:
				break LOOP
			}
		}
		err := writeManifests(manifestOutput, manifests)
		if err != nil {
			ui.Say(fmt.Sprintf("Unable to write out manifest to %s: %s", manifestOutput, err))
		}
	}
	close(amiManifests)

	return
}

// ami encapsulates simplistic details about an AMI.
type ami struct {
	id     string
	region string
}

// amisFromArtifactID returns an AMI slice from a Packer artifact id.
func amisFromArtifactID(artifactID string) (amis []*ami) {
	for _, amiStr := range strings.Split(artifactID, ",") {
		pair := strings.SplitN(amiStr, ":", 2)
		amis = append(amis, &ami{region: pair[0], id: pair[1]})
	}
	return amis
}

func writeManifests(output string, manifests []*AmiManifest) error {
	rawManifest, err := json.Marshal(manifests)
	if err != nil {
		return err
	}
	return os.WriteFile(output, rawManifest, 0644)
}
