package ami_copy

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/bdwyertech/packer-plugin-aws-ami/helpers"

	"github.com/hashicorp/packer-plugin-amazon/builder/common/awserrors"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/retry"
)

// AmiCopy defines the interface to copy images
type AmiCopy interface {
	Copy(ui *packer.Ui) error
	Input() *ec2.CopyImageInput
	Output() *ec2.CopyImageOutput
	Tag(ui *packer.Ui) error
	TargetAccountID() string
}

// AmiCopyImpl holds data and methods related to copying an image.
type AmiCopyImpl struct {
	targetAccountID string
	EC2             *ec2.EC2
	input           *ec2.CopyImageInput
	output          *ec2.CopyImageOutput
	SourceImage     *ec2.Image
	EnsureAvailable bool
	KeepArtifact    bool
	TagsOnly        bool
	Tags            map[string]string
}

// AmiManifest holds the data about the resulting copied image
type AmiManifest struct {
	AccountID string `json:"account_id"`
	Region    string `json:"region"`
	ImageID   string `json:"image_id"`
}

// Copy will perform an EC2 copy based on the `Input` field.
// It will also call Tag to copy the source tags, if any.
func (ac *AmiCopyImpl) Copy(ui *packer.Ui) (err error) {
	if err = ac.input.Validate(); err != nil {
		return err
	}

	if !ac.TagsOnly {
		if ac.output, err = ac.EC2.CopyImage(ac.input); err != nil {
			return err
		}
	} else {
		(*ui).Say(fmt.Sprintf("Only copying tags in %s as tags_only=true", ac.targetAccountID))
		ac.output = (&ec2.CopyImageOutput{}).SetImageId(*ac.input.SourceImageId)
	}

	if err = ac.Tag(ui); err != nil {
		return err
	}

	if ac.EnsureAvailable {
		(*ui).Say("Going to wait for image to be in available state")
		for i := 1; i <= 30; i++ {
			image, err := helpers.LocateSingleAMI(*ac.output.ImageId, ac.EC2)
			if err != nil && image == nil {
				return err
			}
			switch *image.State {
			case ec2.ImageStateAvailable:
				return nil
			case ec2.ImageStateFailed:
				return fmt.Errorf("AMI copy failed: image %s transitioned to failed state on account %s", *image.ImageId, ac.targetAccountID)
			}
			(*ui).Say(fmt.Sprintf("Waiting one minute (%d/30) for AMI to become available, current state: %s for image %s on account %s", i, *image.State, *image.ImageId, ac.targetAccountID))
			time.Sleep(time.Duration(1) * time.Minute)
		}
		return fmt.Errorf("Timed out waiting for image %s to copy to account %s", *ac.output.ImageId, ac.targetAccountID)
	}

	return nil
}

func (ac *AmiCopyImpl) Input() *ec2.CopyImageInput {
	return ac.input
}

func (ac *AmiCopyImpl) SetInput(input *ec2.CopyImageInput) {
	ac.input = input
}

func (ac *AmiCopyImpl) Output() *ec2.CopyImageOutput {
	return ac.output
}

func (ac *AmiCopyImpl) TargetAccountID() string {
	return ac.targetAccountID
}

func (ac *AmiCopyImpl) SetTargetAccountID(id string) {
	ac.targetAccountID = id
}

// Tag will copy tags from the source image to the target (if any).
func (ac *AmiCopyImpl) Tag(ui *packer.Ui) (err error) {
	tags := ac.SourceImage.Tags
	for k, v := range ac.Tags {
		tags = append(tags, &ec2.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}

	if len(tags) == 0 {
		return nil
	}

	(*ui).Say(fmt.Sprintf("Adding tags %v", tags))

	// Retry creating tags for about 2.5 minutes
	ctx := context.TODO()
	return retry.Config{
		Tries: 11,
		ShouldRetry: func(err error) bool {
			return awserrors.Matches(err, "UnauthorizedOperation", "")
		},
		RetryDelay: (&retry.Backoff{InitialBackoff: 200 * time.Millisecond, MaxBackoff: 30 * time.Second, Multiplier: 2}).Linear,
	}.Run(ctx, func(ctx context.Context) error {
		_, err := ac.EC2.CreateTags(&ec2.CreateTagsInput{
			Resources: []*string{ac.output.ImageId},
			Tags:      tags,
		})

		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == "InvalidAMIID.NotFound" ||
				awsErr.Code() == "InvalidSnapshot.NotFound" {
				return nil
			}
		}

		return err
	})
}
