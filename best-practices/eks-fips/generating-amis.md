## Generating FIPS AMI images for EKS

In order to generate FIPS images for EKS, you need to use the Amazon EKS AMI builder: https://github.com/awslabs/amazon-eks-ami

Prerequisites:
- An active AWS subscription
- Valid credentials in ~/.aws/credentials with enough permissions to deploy an EC2 image and store an AMI
- An IAM role with [sufficient permissions](https://github.com/awslabs/amazon-eks-ami/blob/bb3b42e21dd45d4b57270ce89a5a238feb5661ad/doc/usage/overview.md#iam-permissions) to complete the builder process
  * If you have the `nodes.cluster-api-provider-aws.sigs.k8s.io` IAM role from CAPA, that works too.
- The ID of the target subnet to use for the EC2 instance during image building
- GPU instances only: Awareness whether you need the LTS 580 or the latest 595 driver for your GPU type (see [here](https://github.com/awslabs/amazon-eks-ami/blob/bb3b42e21dd45d4b57270ce89a5a238feb5661ad/doc/usage/g7-ami.md))

### Usage

#### For non-GPU instances
```
REGION="eu-west-1"
SUBNET="subnet-0217f3c8863835899"
IAM_ROLE="nodes.cluster-api-provider-aws.sigs.k8s.io"
K8S_VER="1.35"

make k8s="$K8S_VER" aws_region="$REGION" subnet_id="$SUBNET" iam_instance_profile="$IAM_ROLE" os_distro=al2023 enable_fips=true
```

#### For GPU instances
```
REGION="eu-west-1"
SUBNET="subnet-0217f3c8863835899"
IAM_ROLE="nodes.cluster-api-provider-aws.sigs.k8s.io"
K8S_VER="1.35"

make k8s="$K8S_VER" aws_region="$REGION" subnet_id="$SUBNET" iam_instance_profile="$IAM_ROLE" os_distro=al2023 enable_fips=true enable_accelerator=nvidia
```

Note: the builder defaults to an `m5.large` instance for building the image. It can be overridden using the `instance_type` parameter if so desired. However, DO NOT use a GPU instance type to build a GPU AMI. Having a GPU present during the build process can cause the wrong driver version to get picked,even when explictly specifying the NVIDIA driver version to use. You avoid this by having no GPU present during the build process.