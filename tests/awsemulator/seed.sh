#!/usr/bin/env bash
# Seeds a running AWS Emulator with the world the awsemulator contract tests
# read back through Versus's collectors. Everything is created through normal
# AWS API calls with the AWS CLI; the emulator has no seed or reset route, and
# this script ships no data files.
#
# The caller points the AWS CLI at the emulator first:
#   AWS_ENDPOINT_URL   https://127.0.0.1:<port> (loopback only, enforced below)
#   AWS_CA_BUNDLE      the emulator's ca.pem
#   credentials        AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or
#                      AWS_SHARED_CREDENTIALS_FILE, from the emulator's
#                      emulator_credentials file
#   AWS_REGION         us-east-1 (the tests read this region)
# Optional:
#   SEED_WORK_DIR      where the Lambda zip is built (default: a mktemp dir,
#                      removed on exit; a given folder is left in place)
#   SEED_SCALE_BATCHES RunInstances calls of 64 extra instances each
#                      (default 16, so 1024 instances: enough to make
#                      DescribeInstances and DescribeNetworkInterfaces page at
#                      the collector's 1000-item page size). The tests expect
#                      the default; change both together.
#
# It refuses to run unless the endpoint is loopback and STS answers the
# emulator's account, so it can never create anything in a real AWS account.
# Run it once, on a freshly started emulator: names are fixed, so a second run
# fails on the first duplicate.
set -euo pipefail

EMULATOR_ACCOUNT=000000000000
SCALE_BATCHES="${SEED_SCALE_BATCHES:-16}"
TAG_CONTACT='Key=contact,Value=ops-oncall@example.com'

fail() {
  printf 'seed: %s\n' "$*" >&2
  exit 1
}

case "${AWS_ENDPOINT_URL:-}" in
  https://127.0.0.1:* | https://localhost:* | https://\[::1\]:*) ;;
  *) fail "AWS_ENDPOINT_URL must be the local emulator (https://127.0.0.1:<port>), got '${AWS_ENDPOINT_URL:-}'" ;;
esac
[[ -n "${AWS_CA_BUNDLE:-}" && -r "${AWS_CA_BUNDLE}" ]] || fail "AWS_CA_BUNDLE must name the emulator's readable ca.pem"
[[ "${AWS_REGION:-}" == us-east-1 ]] || fail "AWS_REGION must be us-east-1"
[[ "$SCALE_BATCHES" =~ ^[0-9]+$ ]] || fail "SEED_SCALE_BATCHES must be a whole number"
command -v aws >/dev/null || fail "the AWS CLI is not installed"
command -v zip >/dev/null || fail "zip is not installed (needed for the Lambda package)"
export AWS_PAGER="" AWS_EC2_METADATA_DISABLED=true AWS_DEFAULT_REGION=us-east-1

account="$(aws sts get-caller-identity --query Account --output text)"
[[ "$account" == "$EMULATOR_ACCOUNT" ]] || fail "STS answered account '$account', not the emulator's $EMULATOR_ACCOUNT; refusing to seed"

WORK_DIR="${SEED_WORK_DIR:-}"
if [[ -z "$WORK_DIR" ]]; then
  WORK_DIR="$(mktemp -d)"
  # shellcheck disable=SC2064 # expand now: the folder this run created
  trap "rm -rf -- '$WORK_DIR'" EXIT
fi
mkdir -p "$WORK_DIR"

# The database password is random per run and never printed; the emulator
# keeps it in memory only.
db_password="Ct-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"

started=$SECONDS
# step runs one AWS CLI call and prints its text output; any failure stops the
# seed with the step name.
step() {
  local name=$1
  shift
  aws "$@" || fail "step '$name' failed"
}

# background starts a step without waiting; wait_steps waits for all of them
# and fails if any failed. The AWS CLI spends about a second starting up, so
# independent calls run side by side to keep the seed short.
pids=()
background() {
  step "$@" >/dev/null &
  pids+=("$!")
}
wait_steps() {
  local pid failed=0
  for pid in "${pids[@]}"; do
    wait "$pid" || failed=1
  done
  pids=()
  ((failed == 0)) || fail "a parallel step failed (see the step error above)"
}

# Network: one VPC with a public /24 subnet in us-east-1a and a private /20
# subnet in us-east-1b (room for the fleet below: a /24 runs out of addresses,
# as it would on AWS), an attached internet gateway, a route table that routes
# 0.0.0.0/0 to it and is associated with the public subnet, and a NAT gateway.
vpc=$(step vpc ec2 create-vpc --cidr-block 10.42.0.0/16 \
  --tag-specifications "ResourceType=vpc,Tags=[{Key=Name,Value=contract-vpc},{Key=team,Value=platform}]" \
  --query Vpc.VpcId --output text)
public=$(step subnet-public ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.42.1.0/24 --availability-zone us-east-1a \
  --tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=contract-public},{Key=env,Value=test}]" \
  --query Subnet.SubnetId --output text)
private=$(step subnet-private ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.42.16.0/20 --availability-zone us-east-1b \
  --tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=contract-private},{Key=env,Value=test}]" \
  --query Subnet.SubnetId --output text)
igw=$(step igw ec2 create-internet-gateway \
  --tag-specifications "ResourceType=internet-gateway,Tags=[{Key=Name,Value=contract-igw}]" \
  --query InternetGateway.InternetGatewayId --output text)
step igw-attach ec2 attach-internet-gateway --vpc-id "$vpc" --internet-gateway-id "$igw" >/dev/null
rtb=$(step route-table ec2 create-route-table --vpc-id "$vpc" \
  --tag-specifications "ResourceType=route-table,Tags=[{Key=Name,Value=contract-rtb}]" \
  --query RouteTable.RouteTableId --output text)
step route ec2 create-route --route-table-id "$rtb" --destination-cidr-block 0.0.0.0/0 --gateway-id "$igw" >/dev/null
step route-assoc ec2 associate-route-table --route-table-id "$rtb" --subnet-id "$public" >/dev/null
eip=$(step eip ec2 allocate-address --domain vpc --query AllocationId --output text)
step nat ec2 create-nat-gateway --subnet-id "$public" --allocation-id "$eip" \
  --tag-specifications "ResourceType=natgateway,Tags=[{Key=Name,Value=contract-nat}]" >/dev/null
sg=$(step security-group ec2 create-security-group --group-name contract-web --description "contract test web tier" \
  --vpc-id "$vpc" --query GroupId --output text)

# Compute: two tagged instances in the public subnet. Name and contact are
# outside the default tag-value allowlist, so Versus must store them redacted.
ami=$(step image ec2 describe-images --owners amazon --query 'Images[0].ImageId' --output text)
[[ "$ami" == ami-* ]] || fail "no AMI from describe-images (got '$ami')"
web=$(step instance-web ec2 run-instances --image-id "$ami" --instance-type t3.micro --subnet-id "$public" \
  --security-group-ids "$sg" --count 1 \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=contract-web},{Key=app,Value=checkout},{Key=team,Value=payments},{Key=contact,Value=ops-oncall@example.com}]" \
  --query 'Instances[0].InstanceId' --output text)
step instance-worker ec2 run-instances --image-id "$ami" --instance-type m5.large --subnet-id "$public" \
  --security-group-ids "$sg" --count 1 \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=contract-worker},{Key=app,Value=billing},{Key=team,Value=finance}]" \
  --query 'Instances[0].InstanceId' --output text >/dev/null

# CloudFormation: a stack that owns one queue and one topic.
step stack cloudformation create-stack --stack-name contract-stack \
  --template-body '{"AWSTemplateFormatVersion":"2010-09-09","Resources":{"Queue":{"Type":"AWS::SQS::Queue"},"Topic":{"Type":"AWS::SNS::Topic"}}}' \
  --tags Key=app,Value=checkout "$TAG_CONTACT" >/dev/null
step stack-wait cloudformation wait stack-create-complete --stack-name contract-stack

# Data and messaging.
step queue sqs create-queue --queue-name contract-queue >/dev/null
step topic sns create-topic --name contract-topic >/dev/null
step bucket s3api create-bucket --bucket contract-bucket >/dev/null
step table dynamodb create-table --table-name contract-table \
  --attribute-definitions AttributeName=pk,AttributeType=S --key-schema AttributeName=pk,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST >/dev/null

# IAM, Lambda, ECS, EKS.
role_arn=$(step role iam create-role --role-name contract-lambda-role \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}' \
  --query Role.Arn --output text)
printf 'def handler(event, context):\n    return 1\n' >"$WORK_DIR/handler.py"
(cd "$WORK_DIR" && zip -q -X function.zip handler.py) || fail "could not build the Lambda zip"
step function lambda create-function --function-name contract-fn --runtime python3.12 --handler handler.handler \
  --role "$role_arn" --zip-file "fileb://$WORK_DIR/function.zip" >/dev/null
step ecs-cluster ecs create-cluster --cluster-name contract-ecs \
  --tags key=app,value=checkout key=contact,value=ops-oncall@example.com >/dev/null
step eks-cluster eks create-cluster --name contract-eks --role-arn "$role_arn" \
  --resources-vpc-config "subnetIds=$public,$private,securityGroupIds=$sg" \
  --tags app=checkout,contact=ops-oncall@example.com >/dev/null

# Load balancing: an ALB across both subnets that forwards to a target group
# holding the web instance.
alb=$(step load-balancer elbv2 create-load-balancer --name contract-alb --type application \
  --subnets "$public" "$private" --security-groups "$sg" \
  --query 'LoadBalancers[0].LoadBalancerArn' --output text)
tg=$(step target-group elbv2 create-target-group --name contract-tg --protocol HTTP --port 80 \
  --vpc-id "$vpc" --target-type instance --query 'TargetGroups[0].TargetGroupArn' --output text)
step listener elbv2 create-listener --load-balancer-arn "$alb" --protocol HTTP --port 80 \
  --default-actions "Type=forward,TargetGroupArn=$tg" >/dev/null
step targets elbv2 register-targets --target-group-arn "$tg" --targets "Id=$web" >/dev/null

# Database and an alarm on the web instance.
step database rds create-db-instance --db-instance-identifier contract-db --db-instance-class db.t3.micro \
  --engine postgres --master-username contract --master-user-password "$db_password" --allocated-storage 20 \
  --tags Key=app,Value=billing "$TAG_CONTACT" >/dev/null
step alarm cloudwatch put-metric-alarm --alarm-name contract-web-cpu --metric-name CPUUtilization \
  --namespace AWS/EC2 --statistic Average --period 300 --threshold 80 --comparison-operator GreaterThanThreshold \
  --evaluation-periods 1 --dimensions "Name=InstanceId,Value=$web"

# Rule churn: twelve inbound rules added one call at a time, so CloudTrail
# holds more write events than one 50-event LookupEvents page.
port=8001
while ((port <= 8012)); do
  background "ingress-$port" ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port "$port" \
    --cidr 10.0.0.0/8
  port=$((port + 1))
done
wait_steps

# Scale: untagged-for-redaction fleet instances, 64 per call (the emulator's
# per-request cap, as on AWS), so the collector's 1000-item pages continue.
batch=0
while ((batch < SCALE_BATCHES)); do
  background "fleet-$batch" ec2 run-instances --image-id "$ami" --instance-type t3.nano --subnet-id "$private" --count 64 \
    --tag-specifications "ResourceType=instance,Tags=[{Key=env,Value=fleet}]" --query 'length(Instances)' --output text
  batch=$((batch + 1))
done
wait_steps

printf 'seed: done in %d s; vpc=%s web=%s fleet=%d instances\n' "$((SECONDS - started))" "$vpc" "$web" "$((SCALE_BATCHES * 64))"
