#!/bin/sh
#
# Creates the versus-reader role in this AWS account from the JSON policies in
# this directory. The role, trust condition and permissions are the same as the
# CloudFormation template and the Terraform module: metadata reads only, with
# explicit denies for data-plane, secret, log-body and billing surfaces.
#
# The External ID is never echoed. It is written only to a temporary trust
# policy file that is removed by the trap when the script exits.

set -eu

ROLE_NAME=versus-reader
METADATA_POLICY_NAME=versus-reader-metadata-reads
OPTIONAL_POLICY_NAME=versus-reader-optional-data
ROLE_DESCRIPTION="Versus metadata reader: Describe, List and configuration reads only, with explicit denies for data-plane and secret reads."
ROLE_ARN_PATTERN='^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$'

SCRIPT_DIR=$(dirname "$0")

usage() {
	cat <<'USAGE'
Usage: ./create-versus-reader.sh <versus-principal-role-arn> <external-id> [--optional-data]

  versus-principal-role-arn  Exact Versus IAM role ARN from the connect wizard,
                             for example arn:aws:iam::123456789012:role/versus-app.
                             The account root ARN and other non-role principals
                             are refused.

  external-id                The per-customer secret shared with Versus, at
                             least 16 printable ASCII characters without spaces.
                             Pass - to be prompted without echo, so the value
                             never lands in shell history.

  --optional-data            Also attach the optional policy: cost reads
                             (Cost Explorer, Budgets, Cost Optimization Hub,
                             Compute Optimizer), Security Hub findings, AWS
                             Config advanced queries and Trusted Advisor
                             checks. Off by default.

The caller needs iam:CreateRole or iam:UpdateAssumeRolePolicy, iam:PutRolePolicy
and iam:GetRole in the target account. Set AWS_PROFILE or AWS_REGION as usual.
USAGE
}

fail() {
	printf 'error: %s\n' "$1" >&2
	exit 1
}

# Substitute both placeholders in a policy file. The values arrive through the
# environment, so awk escapes backslashes and double quotes for JSON itself and
# no value is ever interpolated into a shell or sed command. The External ID may
# contain any printable ASCII character and must reach the file unchanged.
substitute_placeholders() {
	VERSUS_PRINCIPAL_ARN=$principal VERSUS_EXTERNAL_ID=$external_id awk '
	function json_escape(s,   out, i, c) {
		out = ""
		for (i = 1; i <= length(s); i++) {
			c = substr(s, i, 1)
			if (c == "\\") out = out "\\\\"
			else if (c == "\"") out = out "\\\""
			else out = out c
		}
		return out
	}
	BEGIN {
		principal = json_escape(ENVIRON["VERSUS_PRINCIPAL_ARN"])
		external = json_escape(ENVIRON["VERSUS_EXTERNAL_ID"])
		principal_placeholder = "__VERSUS_PRINCIPAL_ARN__"
		external_placeholder = "__EXTERNAL_ID__"
	}
	{
		line = $0
		while ((i = index(line, principal_placeholder)) > 0)
			line = substr(line, 1, i - 1) principal substr(line, i + length(principal_placeholder))
		while ((i = index(line, external_placeholder)) > 0)
			line = substr(line, 1, i - 1) external substr(line, i + length(external_placeholder))
		print line
	}' "$1"
}

principal=
external_id=
with_optional=0
positional=0

for arg in "$@"; do
	case "$arg" in
	--optional-data)
		with_optional=1
		;;
	-)
		# Sentinel for "prompt for the External ID": handled after the loop.
		external_id=-
		positional=$((positional + 1))
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		usage >&2
		exit 2
		;;
	*)
		case "$positional" in
		0) principal=$arg ;;
		1) external_id=$arg ;;
		*)
			usage >&2
			exit 2
			;;
		esac
		positional=$((positional + 1))
		;;
	esac
done

if [ "$positional" -eq 1 ] || [ "$external_id" = "-" ]; then
	# Read the secret from the terminal without echo so it is not captured in
	# shell history or process arguments.
	if [ ! -t 0 ]; then
		fail "pass the External ID as the second argument when stdin is not a terminal"
	fi
	printf 'External ID: ' >&2
	stty -echo
	trap 'stty echo' EXIT INT TERM
	IFS= read -r external_id || true
	stty echo
	trap - EXIT INT TERM
	printf '\n' >&2
	positional=2
fi

if [ "$positional" -ne 2 ]; then
	usage >&2
	exit 2
fi

# Validate before any AWS call so a malformed principal or External ID never
# reaches the API. The pattern is the same one the template's AllowedPattern
# enforces: an exact role ARN in the aws, aws-cn or aws-us-gov partition.
case "$principal" in
*:root)
	fail "the Versus principal must be an IAM role ARN, not the account root ARN"
	;;
esac
if ! printf '%s\n' "$principal" | awk -v re="$ROLE_ARN_PATTERN" '
	NR == 1 && $0 ~ re { ok = 1; next }
	{ ok = 0 }
	END { exit ok ? 0 : 1 }'; then
	fail "the Versus principal must be an exact IAM role ARN, for example arn:aws:iam::123456789012:role/versus-app"
fi

if [ "${#external_id}" -lt 16 ]; then
	fail "the External ID must be at least 16 characters"
fi
if [ "${#external_id}" -gt 1224 ]; then
	fail "the External ID must be at most 1224 characters"
fi
# Anything outside printable ASCII minus space (0x21-0x7e) is left behind,
# including spaces, tabs and newlines.
if [ "$(printf '%s' "$external_id" | LC_ALL=C tr -d '!-~' | wc -c | tr -d ' ')" -ne 0 ]; then
	fail "the External ID must be printable ASCII without spaces"
fi

if ! command -v aws >/dev/null 2>&1; then
	fail "the AWS CLI (aws) is required but was not found in PATH"
fi
if ! aws sts get-caller-identity >/dev/null 2>&1; then
	fail "no AWS caller identity is available; configure credentials with aws configure or AWS_PROFILE"
fi

tmpdir=$(mktemp -d "${TMPDIR%/}/versus-reader.XXXXXX")
cleanup() {
	rm -rf -- "$tmpdir"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

trust_json=$tmpdir/trust-policy.json
permissions_json=$tmpdir/permissions-policy.json
optional_json=$tmpdir/optional-data-policy.json

substitute_placeholders "$SCRIPT_DIR/trust-policy.json" >"$trust_json"
cp "$SCRIPT_DIR/permissions-policy.json" "$permissions_json"
cp "$SCRIPT_DIR/optional-data-policy.json" "$optional_json"

if aws iam create-role \
	--role-name "$ROLE_NAME" \
	--description "$ROLE_DESCRIPTION" \
	--max-session-duration 3600 \
	--assume-role-policy-document "file://$trust_json" \
	>/dev/null 2>"$tmpdir/create-role.err"; then
	printf 'Created role %s.\n' "$ROLE_NAME"
elif aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
	aws iam update-assume-role-policy \
		--role-name "$ROLE_NAME" \
		--policy-document "file://$trust_json" >/dev/null
	printf 'Role %s already exists; updated its trust policy.\n' "$ROLE_NAME"
else
	cat "$tmpdir/create-role.err" >&2
	fail "could not create role $ROLE_NAME"
fi

aws iam put-role-policy \
	--role-name "$ROLE_NAME" \
	--policy-name "$METADATA_POLICY_NAME" \
	--policy-document "file://$permissions_json" >/dev/null
printf 'Attached inline policy %s.\n' "$METADATA_POLICY_NAME"

if [ "$with_optional" -eq 1 ]; then
	aws iam put-role-policy \
		--role-name "$ROLE_NAME" \
		--policy-name "$OPTIONAL_POLICY_NAME" \
		--policy-document "file://$optional_json" >/dev/null
	printf 'Attached inline policy %s.\n' "$OPTIONAL_POLICY_NAME"
elif aws iam get-role-policy --role-name "$ROLE_NAME" --policy-name "$OPTIONAL_POLICY_NAME" >/dev/null 2>&1; then
	printf 'Note: %s is still attached from an earlier run. To match this run, remove it with:\n' "$OPTIONAL_POLICY_NAME"
	printf '  aws iam delete-role-policy --role-name %s --policy-name %s\n' "$ROLE_NAME" "$OPTIONAL_POLICY_NAME"
fi

role_arn=$(aws iam get-role --role-name "$ROLE_NAME" --query 'Role.Arn' --output text)

printf '\nRole ARN: %s\n' "$role_arn"
printf 'Paste it into the Versus connect wizard, then verify access with:\n'
printf '  aws sts assume-role --role-arn %s --role-session-name versus-verify --external-id <external-id>\n' "$role_arn"
