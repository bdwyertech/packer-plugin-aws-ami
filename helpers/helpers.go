package helpers

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// LocateSingleAMI tries to locate a single AMI for the given ID.
func LocateSingleAMI(id string, ec2Conn *ec2.EC2) (*ec2.Image, error) {
	if output, err := ec2Conn.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("image-id"),
				Values: aws.StringSlice([]string{id}),
			},
		},
	}); err != nil {
		return nil, err
	} else if len(output.Images) != 1 {
		return nil, fmt.Errorf("Single source image not located (found: %d images)",
			len(output.Images))
	} else {
		return output.Images[0], nil
	}
}
