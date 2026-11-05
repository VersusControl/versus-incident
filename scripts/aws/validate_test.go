package awsdeploy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	templateFile = "versus-reader.yaml"
	terraformDir = "terraform/versus-reader"
	optionalGate = "IncludeOptionalData"
	cliDir       = "cli"
)

// expectedAllowActions is the metadata-only allow list: the service catalog
// probes in the plan's service table, the shared inventory helpers and the S3
// security metadata reads. Nothing here can read object bodies, secret values
// or log events, and nothing here writes.
var expectedAllowActions = []string{
	"apigateway:GET",
	"autoscaling:Describe*",
	"health:Describe*",
	"cloudformation:Describe*",
	"cloudfront:List*",
	"cloudtrail:LookupEvents",
	"cloudwatch:DescribeAlarms*",
	"config:Describe*",
	"dynamodb:DescribeTable",
	"dynamodb:ListTables",
	"ec2:Describe*",
	"ecs:Describe*",
	"ecs:List*",
	"eks:Describe*",
	"eks:List*",
	"elasticache:Describe*",
	"elasticloadbalancing:Describe*",
	"events:Describe*",
	"events:List*",
	"iam:GetRole",
	"iam:List*",
	"kinesis:Describe*",
	"kinesis:List*",
	"lambda:GetFunctionConfiguration",
	"lambda:List*",
	"rds:Describe*",
	"route53:GetHealthCheckStatus",
	"route53:List*",
	"s3:GetBucketLocation",
	"s3:GetBucketPolicyStatus",
	"s3:GetBucketPublicAccessBlock",
	"s3:GetEncryptionConfiguration",
	"s3:ListAllMyBuckets",
	"sns:GetTopicAttributes",
	"sns:List*",
	"sqs:GetQueueAttributes",
	"sqs:List*",
	"states:Describe*",
	"states:List*",
	"resource-explorer-2:Search",
	"servicecatalog:ListApplications",
	"tag:GetResources",
}

// optionalActions are added only when the OptionalData parameter is on.
var optionalActions = []string{
	"ce:GetAnomalies",
	"ce:GetCostAndUsage",
	"securityhub:GetFindings",
	"config:SelectResourceConfig",
	"trustedadvisor:Describe*",
}

type denyGroup struct {
	name    string
	actions []string
}

// billingDenyActions is the billing-surface deny group. It is modelled
// separately because it is a distinct statement in both artifacts.
var billingDenyActions = []string{"aws-portal:*", "billing:*", "invoicing:*", "payments:*"}

// expectedDenyGroups is the nine explicit deny groups. The deny wins over any
// allow, so a broader policy attached later cannot open these reads.
var expectedDenyGroups = []denyGroup{
	{name: "object bodies", actions: []string{"s3:GetObject"}},
	{name: "secret values", actions: []string{"secretsmanager:GetSecretValue"}},
	{name: "parameter values", actions: []string{"ssm:GetParameter*"}},
	{name: "decrypted blobs", actions: []string{"kms:Decrypt"}},
	{name: "table items", actions: []string{"dynamodb:GetItem", "dynamodb:Query", "dynamodb:Scan"}},
	{name: "function code", actions: []string{"lambda:GetFunction"}},
	{name: "instance console output", actions: []string{"ec2:GetConsoleOutput"}},
	{name: "log events", actions: []string{"logs:GetLogEvents"}},
	{name: "billing surfaces", actions: billingDenyActions},
}

func TestTemplateAllowListIsExplicit(t *testing.T) {
	parsed := parseTemplate(t)

	if missing, extra := diffSets(toSet(expectedAllowActions), parsed.allow); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("allow list drifted from the catalog table: missing=%v extra=%v", missing, extra)
	}
	for action := range parsed.allow {
		if action == "*" || strings.HasSuffix(action, ":*") {
			t.Errorf("allow list entry %q is too broad; name the actions explicitly", action)
		}
	}
}

func TestDenyGroupsPresentInBothArtifacts(t *testing.T) {
	parsed := parseTemplate(t)
	mainTF := readArtifact(t, terraformDir, "main.tf")
	tfDeny := toSet(quotedActions(t, blockBody(t, mainTF, `data "aws_iam_policy_document" "deny_reads"`)))

	for _, group := range expectedDenyGroups {
		for _, action := range group.actions {
			if !parsed.deny[action] {
				t.Errorf("CloudFormation template is missing deny %q (%s)", action, group.name)
			}
			if !tfDeny[action] {
				t.Errorf("Terraform module is missing deny %q (%s)", action, group.name)
			}
		}
	}
	for action := range parsed.deny {
		if parsed.allow[action] {
			t.Errorf("action %q is both allowed and denied", action)
		}
		if parsed.conditionalAll()[action] {
			t.Errorf("action %q is both optional-allowed and denied", action)
		}
	}
}

// TestTerraformDenySetMatchesCloudFormation reads the Terraform deny policy as
// statements, so a dropped statement (for example the billing-surface group)
// fails on parity rather than only on a per-action presence check.
func TestTerraformDenySetMatchesCloudFormation(t *testing.T) {
	parsed := parseTemplate(t)
	mainTF := readArtifact(t, terraformDir, "main.tf")

	statements := terraformDenyStatements(t, mainTF)
	if len(statements) == 0 {
		t.Fatal("Terraform deny_reads policy has no statement blocks")
	}

	tfDeny := map[string]bool{}
	bySID := map[string][]string{}
	for _, statement := range statements {
		if statement.sid == "" {
			t.Error("Terraform deny statement has no sid")
		}
		if _, seen := bySID[statement.sid]; seen {
			t.Errorf("Terraform deny statement sid %q is declared twice", statement.sid)
		}
		bySID[statement.sid] = statement.actions
		for _, action := range statement.actions {
			tfDeny[action] = true
		}
	}

	for _, group := range expectedDenyGroups {
		for _, action := range group.actions {
			if !tfDeny[action] {
				t.Errorf("Terraform deny set is missing %q (%s)", action, group.name)
			}
			if !parsed.deny[action] {
				t.Errorf("CloudFormation deny set is missing %q (%s)", action, group.name)
			}
		}
	}

	const billingSID = "DenyBillingAccountSurfaces"
	billing, ok := bySID[billingSID]
	if !ok {
		t.Errorf("Terraform deny policy has no %s statement; billing account surfaces would be readable", billingSID)
	} else if missing, extra := diffSets(toSet(billingDenyActions), toSet(billing)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("%s actions drifted: missing=%v extra=%v", billingSID, missing, extra)
	}

	if missing, extra := diffSets(parsed.deny, tfDeny); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("Terraform and CloudFormation deny sets are not identical: missing from Terraform=%v extra in Terraform=%v", missing, extra)
	}
}

func TestTrustPolicyRequiresExternalID(t *testing.T) {
	parsed := parseTemplate(t)

	if parsed.trust == nil {
		t.Fatal("template has no assume-role trust statement")
	}
	if got := scalarOf(t, parsed.trust, "Effect"); got != "Allow" {
		t.Errorf("trust statement Effect = %q, want Allow", got)
	}
	if actions := actionStrings(t, parsed.trust); len(actions) != 1 || actions[0] != "sts:AssumeRole" {
		t.Errorf("trust statement actions = %v, want [sts:AssumeRole]", actions)
	}

	principal := mappingValue(t, parsed.trust, "Principal")
	awsPrincipal := mappingValue(t, principal, "AWS")
	if awsPrincipal == nil || awsPrincipal.Value != "VersusPrincipalArn" {
		t.Fatalf("trust statement principal must reference the VersusPrincipalArn parameter")
	}
	if awsPrincipal.Kind != yaml.ScalarNode {
		t.Fatalf("trust statement principal must be one parameter reference, not a list")
	}
	if strings.Contains(awsPrincipal.Value, "*") {
		t.Errorf("trust statement principal %q uses a wildcard", awsPrincipal.Value)
	}

	condition := mappingValue(t, parsed.trust, "Condition")
	if condition == nil {
		t.Fatal("trust statement lacks a Condition block")
	}
	for i := 0; i+1 < len(condition.Content); i += 2 {
		if key := condition.Content[i].Value; key != "StringEquals" {
			t.Errorf("trust condition operator = %q, want StringEquals; StringLike would allow wildcards", key)
		}
	}
	stringEquals := mappingValue(t, condition, "StringEquals")
	externalID := mappingValue(t, stringEquals, "sts:ExternalId")
	if externalID == nil {
		t.Fatal("trust statement lacks a StringEquals sts:ExternalId condition")
	}
	if externalID.Value != "ExternalId" {
		t.Errorf("sts:ExternalId condition = %q, want a reference to the ExternalId parameter", externalID.Value)
	}
	if len(stringEquals.Content) != 2 {
		t.Errorf("trust condition must contain only the sts:ExternalId match, got %d entries", len(stringEquals.Content)/2)
	}

	params := mappingValue(t, parsed.root, "Parameters")
	externalParam := mappingValue(t, params, "ExternalId")
	if externalParam == nil {
		t.Fatal("template lacks an ExternalId parameter")
	}
	if got := scalarOf(t, externalParam, "NoEcho"); got != "true" {
		t.Errorf("ExternalId NoEcho = %q, want true", got)
	}

	mainTF := readArtifact(t, terraformDir, "main.tf")
	trustBlock := blockBody(t, mainTF, `data "aws_iam_policy_document" "assume_role"`)
	for _, want := range []string{`"sts:AssumeRole"`, `"sts:ExternalId"`, "var.external_id", "var.versus_principal_arn"} {
		if !strings.Contains(trustBlock, want) {
			t.Errorf("Terraform trust policy lacks %s", want)
		}
	}
	if !regexp.MustCompile(`(?m)^\s*test\s*=\s*"StringEquals"\s*$`).MatchString(trustBlock) {
		t.Error("Terraform trust condition must use StringEquals")
	}
	if !regexp.MustCompile(`identifiers\s*=\s*\[var\.versus_principal_arn\]`).MatchString(trustBlock) {
		t.Error("Terraform trust principal must be exactly [var.versus_principal_arn], with no wildcard")
	}
	if strings.Contains(trustBlock, `"*"`) {
		t.Error("Terraform trust policy must not contain a wildcard principal")
	}
}

// roleARNPattern is the only principal form both artifacts accept: an IAM role
// ARN in any partition. The account root is rejected because trusting it
// delegates the role to every principal in the Versus account.
const roleARNPattern = `^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`

// roleNameCharset is the IAM role-name character class. Dropping it lets a
// wildcard, a separator or the account root through the Terraform validation.
const roleNameCharset = `[A-Za-z0-9+=,.@_/-]`

var validRoleARNs = []string{
	"arn:aws:iam::123456789012:role/versus-app",
	"arn:aws-cn:iam::123456789012:role/versus-app",
	"arn:aws-us-gov:iam::123456789012:role/versus-app",
	"arn:aws:iam::123456789012:role/versus/reader-Role_1@example.com",
	"arn:aws:iam::123456789012:role/versus-app+name=",
}

var nonRolePrincipals = []string{
	"arn:aws:iam::123456789012:root",
	"arn:aws:iam::123456789012:user/versus",
	"arn:aws:iam::123456789012:group/versus",
	"arn:aws:iam::123456789012:federated-user/versus",
	"arn:aws:iam::123456789012:role/",
	"arn:aws:iam::*:role/versus-app",
	"arn:aws:iam::123456789012:role/versus-*",
	"arn:aws:iam::12345678901:role/versus-app",
	"*",
	"",
}

func TestPrincipalArnPatternAcceptsRoleARNsOnly(t *testing.T) {
	parsed := parseTemplate(t)

	params := mappingValue(t, parsed.root, "Parameters")
	principalParam := mappingValue(t, params, "VersusPrincipalArn")
	if principalParam == nil {
		t.Fatal("template lacks a VersusPrincipalArn parameter")
	}
	cfnPattern := scalarOf(t, principalParam, "AllowedPattern")
	if cfnPattern == "" {
		t.Fatal("VersusPrincipalArn has no AllowedPattern")
	}
	if got := scalarOf(t, principalParam, "ConstraintDescription"); got == "" {
		t.Error("VersusPrincipalArn has no ConstraintDescription")
	}

	variables := readArtifact(t, terraformDir, "variables.tf")
	tfPattern := terraformRegexp(t, blockBody(t, variables, `variable "versus_principal_arn"`))
	if tfPattern == "" {
		t.Fatal("versus_principal_arn has no validation regexp")
	}

	if cfnPattern != tfPattern {
		t.Errorf("principal ARN patterns diverged: CloudFormation %q, Terraform %q", cfnPattern, tfPattern)
	}
	if cfnPattern != roleARNPattern {
		t.Errorf("principal ARN pattern = %q, want %q", cfnPattern, roleARNPattern)
	}

	for _, pattern := range []string{cfnPattern, tfPattern} {
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("principal ARN pattern %q does not compile: %v", pattern, err)
		}
		for _, arn := range validRoleARNs {
			if !re.MatchString(arn) {
				t.Errorf("pattern %q rejects the valid role ARN %q", pattern, arn)
			}
		}
		for _, arn := range nonRolePrincipals {
			if re.MatchString(arn) {
				t.Errorf("pattern %q accepts the non-role principal %q", pattern, arn)
			}
		}
		if re.MatchString("arn:aws:iam::123456789012:root") {
			t.Errorf("pattern %q accepts the account root ARN, delegating the role to the whole Versus account", pattern)
		}
	}
}

// TestTerraformPrincipalArnValidationKeepsCharset reads the Terraform
// validation block itself, so a removed block or a pattern that loses the
// role-name charset fails even when no other artifact changed.
func TestTerraformPrincipalArnValidationKeepsCharset(t *testing.T) {
	variables := readArtifact(t, terraformDir, "variables.tf")
	variable := blockBody(t, variables, `variable "versus_principal_arn"`)

	validation := terraformValidationBlock(t, variable)
	if validation == "" {
		t.Fatal("versus_principal_arn has no validation block; Terraform would accept the account root or a wildcard principal")
	}
	validation = stripHCLComments(validation)

	pattern := terraformRegexp(t, validation)
	if pattern == "" {
		t.Fatal("versus_principal_arn validation has no can(regex(...)) condition")
	}
	if !strings.Contains(validation, "error_message") {
		t.Error("versus_principal_arn validation has no error_message")
	}
	if !strings.Contains(validation, "var.versus_principal_arn") {
		t.Error("versus_principal_arn validation must test the variable value")
	}

	wantSuffix := ":role/" + roleNameCharset + "{1,512}$"
	if !strings.HasSuffix(pattern, wantSuffix) {
		t.Errorf("versus_principal_arn pattern = %q, want it to end with %q so only role names pass", pattern, wantSuffix)
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("versus_principal_arn validation pattern %q does not compile: %v", pattern, err)
	}
	for _, arn := range validRoleARNs {
		if !re.MatchString(arn) {
			t.Errorf("versus_principal_arn pattern %q rejects the valid role ARN %q", pattern, arn)
		}
	}
	for _, arn := range nonRolePrincipals {
		if re.MatchString(arn) {
			t.Errorf("versus_principal_arn pattern %q accepts the non-role principal %q", pattern, arn)
		}
	}
	if re.MatchString("arn:aws:iam::123456789012:root") {
		t.Error("versus_principal_arn pattern accepts the account root ARN")
	}
}

func TestExternalIDMinimumLengthMatchesInBothArtifacts(t *testing.T) {
	parsed := parseTemplate(t)

	params := mappingValue(t, parsed.root, "Parameters")
	externalParam := mappingValue(t, params, "ExternalId")
	if externalParam == nil {
		t.Fatal("template lacks an ExternalId parameter")
	}
	if got := scalarOf(t, externalParam, "MinLength"); got != "16" {
		t.Errorf("ExternalId MinLength = %q, want 16", got)
	}
	if got := scalarOf(t, externalParam, "AllowedPattern"); got != `^[!-~]{16,1224}$` {
		t.Errorf("ExternalId AllowedPattern = %q, want ^[!-~]{16,1224}$", got)
	}
	if got := scalarOf(t, externalParam, "ConstraintDescription"); !strings.Contains(got, "16") {
		t.Errorf("ExternalId ConstraintDescription %q must state the 16 character minimum", got)
	}

	variables := readArtifact(t, terraformDir, "variables.tf")
	validation := blockBody(t, variables, `variable "external_id"`)
	if !regexp.MustCompile(`length\(var\.external_id\)\s*>=\s*16`).MatchString(validation) {
		t.Error("Terraform external_id validation must require at least 16 characters")
	}
	if !regexp.MustCompile(`length\(var\.external_id\)\s*<=\s*1224`).MatchString(validation) {
		t.Error("Terraform external_id validation must cap the value at 1224 characters")
	}
	if !strings.Contains(validation, "16") {
		t.Error("Terraform external_id error message must state the 16 character minimum")
	}
}

func TestDescriptionsAndDocsRejectAccountRoot(t *testing.T) {
	parsed := parseTemplate(t)
	params := mappingValue(t, parsed.root, "Parameters")
	principalParam := mappingValue(t, params, "VersusPrincipalArn")
	principalDesc := strings.ToLower(scalarOf(t, principalParam, "Description"))

	variables := readArtifact(t, terraformDir, "variables.tf")
	tfDesc := strings.ToLower(terraformAttribute(t, blockBody(t, variables, `variable "versus_principal_arn"`), "description"))

	readme := readArtifact(t, "README.md")

	if !warnsAgainstRoot(principalDesc) {
		t.Errorf("VersusPrincipalArn description must say not to use the account root, got %q", principalDesc)
	}
	if !warnsAgainstRoot(tfDesc) {
		t.Errorf("versus_principal_arn description must say not to use the account root, got %q", tfDesc)
	}
	if !strings.Contains(principalDesc, "role") || !strings.Contains(tfDesc, "role") {
		t.Error("both principal descriptions must say the value is a Versus role ARN")
	}

	// Every documentation block that mentions the account root must warn
	// against it, and no runnable example may pass a root ARN.
	for _, block := range strings.Split(readme, "\n\n") {
		lower := strings.ToLower(block)
		if !strings.Contains(lower, "account root") && !strings.Contains(lower, ":root") {
			continue
		}
		if !warnsAgainstRoot(lower) {
			t.Errorf("README block mentions the account root without warning against it:\n%s", block)
		}
	}
	if rootPrincipalDoc.MatchString(readme) {
		t.Error("README example passes an account root ARN as versus_principal_arn")
	}
}

var rootPrincipalDoc = regexp.MustCompile(`versus_principal_arn=arn:aws[a-z-]*:iam::[0-9]{12}:root`)

func warnsAgainstRoot(text string) bool {
	lower := strings.ReplaceAll(strings.ToLower(text), "*", "")
	return strings.Contains(lower, "not ") || strings.Contains(lower, "never") || strings.Contains(lower, "no ")
}

var terraformRegexpCall = regexp.MustCompile(`regex\(\s*"([^"]+)"`)

func terraformRegexp(t *testing.T, body string) string {
	t.Helper()

	match := terraformRegexpCall.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

func terraformAttribute(t *testing.T, body, name string) string {
	t.Helper()

	match := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*"([^"]*)"`).FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

// terraformValidationBlock returns the body of the first validation block in a
// variable body, or "" when the variable declares none.
func terraformValidationBlock(t *testing.T, body string) string {
	t.Helper()

	start := strings.Index(body, "validation")
	if start < 0 {
		return ""
	}
	return blockBody(t, body[start:], "validation")
}

// stripHCLComments removes whole-line and trailing # comments so a commented
// out rule cannot satisfy an assertion.
func stripHCLComments(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if cut := strings.Index(line, "#"); cut >= 0 {
			lines[i] = line[:cut]
		}
	}
	return strings.Join(lines, "\n")
}

// terraformDenyStatement is one Deny statement of the Terraform deny policy.
type terraformDenyStatement struct {
	sid     string
	actions []string
}

// terraformDenyStatements splits the Terraform deny policy into its Deny
// statements so each group, including the billing group, is checked on its own.
func terraformDenyStatements(t *testing.T, mainTF string) []terraformDenyStatement {
	t.Helper()

	document := stripHCLComments(blockBody(t, mainTF, `data "aws_iam_policy_document" "deny_reads"`))
	var out []terraformDenyStatement
	for offset := 0; ; {
		rel := strings.Index(document[offset:], "statement")
		if rel < 0 {
			return out
		}
		start := offset + rel
		body := blockBody(t, document[start:], "statement")
		if strings.Contains(body, `effect = "Deny"`) {
			out = append(out, terraformDenyStatement{
				sid:     terraformAttribute(t, body, "sid"),
				actions: quotedActions(t, body),
			})
		}
		offset = start + len("statement")
	}
}

func TestOptionalDataIsOffByDefaultAndConditional(t *testing.T) {
	parsed := parseTemplate(t)

	params := mappingValue(t, parsed.root, "Parameters")
	optionalParam := mappingValue(t, params, "OptionalData")
	if optionalParam == nil {
		t.Fatal("template lacks an OptionalData parameter")
	}
	if got := scalarOf(t, optionalParam, "Default"); got != "off" {
		t.Errorf("OptionalData Default = %q, want off", got)
	}
	if got := scalarValues(t, mappingValue(t, optionalParam, "AllowedValues")); !sameSet(got, []string{"off", "on"}) {
		t.Errorf("OptionalData AllowedValues = %v, want [off on]", got)
	}

	conditions := mappingValue(t, parsed.root, "Conditions")
	gate := mappingValue(t, conditions, optionalGate)
	if gate == nil || gate.Tag != "!Equals" {
		t.Fatalf("Conditions.%s must be an !Equals test", optionalGate)
	}
	args := gate.Content
	if len(args) != 2 {
		t.Fatalf("Conditions.%s must compare two values", optionalGate)
	}
	pair := args
	if pair[0].Value != "OptionalData" || pair[1].Value != "on" {
		t.Errorf("Conditions.%s compares %q to %q, want OptionalData to on", optionalGate, pair[0].Value, pair[1].Value)
	}

	got := parsed.conditionalActions[optionalGate]
	if missing, extra := diffSets(toSet(optionalActions), got); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("optional allow list drifted: missing=%v extra=%v", missing, extra)
	}
	for _, action := range optionalActions {
		if parsed.allow[action] {
			t.Errorf("optional action %q is allowed without the OptionalData condition", action)
		}
	}
	for condition, actions := range parsed.conditionalActions {
		if condition != optionalGate {
			t.Errorf("allow actions %v are gated by unknown condition %q", keys(actions), condition)
		}
	}

	variables := readArtifact(t, terraformDir, "variables.tf")
	optionalVar := blockBody(t, variables, `variable "optional_data"`)
	if !regexp.MustCompile(`(?m)^\s*default\s*=\s*false\s*$`).MatchString(optionalVar) {
		t.Error("Terraform optional_data must default to false")
	}
	mainTF := readArtifact(t, terraformDir, "main.tf")
	optionalTF := blockBody(t, mainTF, `data "aws_iam_policy_document" "optional_data"`)
	if missing, extra := diffSets(toSet(optionalActions), toSet(quotedActions(t, optionalTF))); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("Terraform optional actions drifted: missing=%v extra=%v", missing, extra)
	}
	attachBlock := blockBody(t, mainTF, `resource "aws_iam_role_policy" "optional_data"`)
	if !strings.Contains(attachBlock, "count = var.optional_data ? 1 : 0") {
		t.Error("Terraform optional policy must be gated by var.optional_data")
	}
}

func TestTerraformModuleMatchesTemplate(t *testing.T) {
	parsed := parseTemplate(t)
	mainTF := readArtifact(t, terraformDir, "main.tf")

	metadataTF := toSet(quotedActions(t, blockBody(t, mainTF, `data "aws_iam_policy_document" "metadata_reads"`)))
	if missing, extra := diffSets(parsed.allow, metadataTF); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("Terraform metadata allow list differs from the template: missing=%v extra=%v", missing, extra)
	}

	tfDeny := toSet(quotedActions(t, blockBody(t, mainTF, `data "aws_iam_policy_document" "deny_reads"`)))
	for _, group := range expectedDenyGroups {
		for _, action := range group.actions {
			if !tfDeny[action] {
				t.Errorf("Terraform deny list is missing %q (%s)", action, group.name)
			}
		}
	}

	if !strings.Contains(mainTF, `resource "aws_iam_role" "versus_reader"`) {
		t.Error("Terraform module must create the role with aws_iam_role")
	}
	if !strings.Contains(mainTF, `resource "aws_iam_role_policy" "metadata_reads"`) {
		t.Error("Terraform module must attach the inline policy with aws_iam_role_policy")
	}
}

func TestTerraformVariablesAndOutputs(t *testing.T) {
	variables := readArtifact(t, terraformDir, "variables.tf")
	for _, name := range []string{"versus_principal_arn", "external_id", "optional_data"} {
		blockBody(t, variables, fmt.Sprintf(`variable %q`, name))
	}
	if !strings.Contains(blockBody(t, variables, `variable "external_id"`), "sensitive") {
		t.Error("Terraform external_id must be marked sensitive")
	}

	outputs := readArtifact(t, terraformDir, "outputs.tf")
	blockBody(t, outputs, `output "role_arn"`)
	blockBody(t, outputs, `output "role_name"`)
}

func TestTemplateResourcesAndOutputsAreDocumented(t *testing.T) {
	parsed := parseTemplate(t)

	resources := mappingValue(t, parsed.root, "Resources")
	if resources == nil {
		t.Fatal("template has no Resources section")
	}
	if len(resources.Content) == 0 {
		t.Fatal("template has no resources")
	}
	for i := 0; i+1 < len(resources.Content); i += 2 {
		name := resources.Content[i].Value
		body := resources.Content[i+1]
		if mappingValue(t, body, "Description") == nil {
			t.Errorf("resource %q has no Description", name)
		}
	}

	outputs := mappingValue(t, parsed.root, "Outputs")
	for _, name := range []string{"RoleArn", "RoleName"} {
		output := mappingValue(t, outputs, name)
		if output == nil {
			t.Errorf("template lacks output %q", name)
			continue
		}
		if mappingValue(t, output, "Value") == nil {
			t.Errorf("output %q has no Value", name)
		}
		if mappingValue(t, output, "Description") == nil {
			t.Errorf("output %q has no Description", name)
		}
	}
}

type templatePolicy struct {
	root               *yaml.Node
	allow              map[string]bool
	conditionalActions map[string]map[string]bool
	deny               map[string]bool
	trust              *yaml.Node
}

func (p templatePolicy) conditionalAll() map[string]bool {
	out := map[string]bool{}
	for _, actions := range p.conditionalActions {
		for action := range actions {
			out[action] = true
		}
	}
	return out
}

func parseTemplate(t *testing.T) templatePolicy {
	t.Helper()

	data, err := os.ReadFile(templateFile)
	if err != nil {
		t.Fatalf("read %s: %v", templateFile, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", templateFile, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("%s is empty", templateFile)
	}

	parsed := templatePolicy{
		root:               doc.Content[0],
		allow:              map[string]bool{},
		conditionalActions: map[string]map[string]bool{},
		deny:               map[string]bool{},
	}

	resources := mappingValue(t, parsed.root, "Resources")
	role := mappingValue(t, resources, "ReaderRole")
	if role == nil {
		t.Fatal("template has no ReaderRole resource")
	}
	properties := mappingValue(t, role, "Properties")

	assume := mappingValue(t, properties, "AssumeRolePolicyDocument")
	for _, statement := range statementList(t, assume) {
		if statement.condition != "" {
			t.Errorf("assume-role trust policy must not be conditional on %q", statement.condition)
			continue
		}
		if parsed.trust != nil {
			t.Error("assume-role trust policy must have exactly one statement")
		}
		parsed.trust = statement.node
	}

	policies := mappingValue(t, properties, "Policies")
	if policies == nil || policies.Kind != yaml.SequenceNode {
		t.Fatal("ReaderRole has no inline Policies list")
	}
	for _, policy := range policies.Content {
		document := mappingValue(t, policy, "PolicyDocument")
		for _, statement := range statementList(t, document) {
			effect := scalarOf(t, statement.node, "Effect")
			actions := actionStrings(t, statement.node)
			for _, action := range actions {
				switch {
				case effect == "Deny":
					parsed.deny[action] = true
				case effect == "Allow" && statement.condition == "":
					parsed.allow[action] = true
				case effect == "Allow":
					if parsed.conditionalActions[statement.condition] == nil {
						parsed.conditionalActions[statement.condition] = map[string]bool{}
					}
					parsed.conditionalActions[statement.condition][action] = true
				default:
					t.Errorf("statement action %q has Effect %q, want Allow or Deny", action, effect)
				}
			}
		}
	}
	return parsed
}

type templateStatement struct {
	condition string
	node      *yaml.Node
}

func statementList(t *testing.T, document *yaml.Node) []templateStatement {
	t.Helper()

	if document == nil {
		return nil
	}
	statements := mappingValue(t, document, "Statement")
	if statements == nil {
		return nil
	}
	if statements.Kind == yaml.MappingNode {
		return []templateStatement{{node: statements}}
	}
	if statements.Kind != yaml.SequenceNode {
		t.Fatalf("Statement must be a mapping or a list, got kind %d", statements.Kind)
	}

	var out []templateStatement
	for _, item := range statements.Content {
		switch {
		case item.Kind == yaml.MappingNode:
			out = append(out, templateStatement{node: item})
		case item.Tag == "!If":
			if item.Kind != yaml.SequenceNode || len(item.Content) != 3 {
				t.Fatalf("!If statement must take a condition, a body and a fallback")
			}
			condition := item.Content[0].Value
			body := item.Content[1]
			if body.Tag == "!Ref" {
				continue
			}
			if body.Kind != yaml.MappingNode {
				t.Fatalf("!If statement body must be a mapping")
			}
			out = append(out, templateStatement{condition: condition, node: body})
		default:
			t.Fatalf("unsupported Statement entry: tag %q kind %d", item.Tag, item.Kind)
		}
	}
	return out
}

func actionStrings(t *testing.T, statement *yaml.Node) []string {
	t.Helper()

	action := mappingValue(t, statement, "Action")
	if action == nil {
		action = mappingValue(t, statement, "NotAction")
	}
	switch {
	case action == nil:
		return nil
	case action.Kind == yaml.ScalarNode:
		return []string{action.Value}
	case action.Kind == yaml.SequenceNode:
		out := make([]string, 0, len(action.Content))
		for _, item := range action.Content {
			out = append(out, item.Value)
		}
		return out
	default:
		t.Fatalf("Action must be a string or a list")
		return nil
	}
}

func mappingValue(t *testing.T, node *yaml.Node, key string) *yaml.Node {
	t.Helper()

	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func scalarOf(t *testing.T, node *yaml.Node, key string) string {
	t.Helper()

	value := mappingValue(t, node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}

func scalarValues(t *testing.T, node *yaml.Node) []string {
	t.Helper()

	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		return []string{node.Value}
	}
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		out = append(out, item.Value)
	}
	return out
}

func readArtifact(t *testing.T, parts ...string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(data)
}

var quotedAction = regexp.MustCompile(`"([a-z0-9-]+:[A-Za-z0-9*]+)"`)

func quotedActions(t *testing.T, body string) []string {
	t.Helper()

	if body == "" {
		t.Fatal("empty Terraform block")
	}
	var out []string
	for _, match := range quotedAction.FindAllStringSubmatch(body, -1) {
		out = append(out, match[1])
	}
	return out
}

func blockBody(t *testing.T, src, header string) string {
	t.Helper()

	start := strings.Index(src, header)
	if start < 0 {
		t.Fatalf("Terraform source lacks block %q", header)
	}
	openRel := strings.Index(src[start+len(header):], "{")
	if openRel < 0 {
		t.Fatalf("Terraform block %q has no opening brace", header)
	}
	open := start + len(header) + openRel

	depth := 0
	inString := false
	for i := open; i < len(src); i++ {
		c := src[i]
		if inString {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '#':
			if next := strings.IndexByte(src[i:], '\n'); next >= 0 {
				i += next
			} else {
				return src[open+1:]
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open+1 : i]
			}
		}
	}
	t.Fatalf("Terraform block %q is not closed", header)
	return ""
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func sameSet(a, b []string) bool {
	missing, extra := diffSets(toSet(a), toSet(b))
	return len(missing) == 0 && len(extra) == 0
}

func diffSets(want, got map[string]bool) (missing, extra []string) {
	for item := range want {
		if !got[item] {
			missing = append(missing, item)
		}
	}
	for item := range got {
		if !want[item] {
			extra = append(extra, item)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// The CLI path builds the same role from JSON policy files. These types decode
// those files so the tests compare statement by statement, not by text.

type cliPolicyDocument struct {
	Version   string         `json:"Version"`
	Statement []cliStatement `json:"Statement"`
}

type cliStatement struct {
	Sid       string                       `json:"Sid"`
	Effect    string                       `json:"Effect"`
	Action    stringList                   `json:"Action"`
	Resource  string                       `json:"Resource"`
	Principal map[string]string            `json:"Principal"`
	Condition map[string]map[string]string `json:"Condition"`
}

// stringList accepts both the scalar and the list form of an IAM Action.
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

func readCLIPolicy(t *testing.T, name string) cliPolicyDocument {
	t.Helper()

	body := readArtifact(t, cliDir, name)
	var policy cliPolicyDocument
	if err := json.Unmarshal([]byte(body), &policy); err != nil {
		t.Fatalf("parse %s/%s: %v", cliDir, name, err)
	}
	return policy
}

// templatePolicyStatement is one statement of the template's inline role
// policy. condition is set for statements inside an !If, such as the optional
// data allow.
type templatePolicyStatement struct {
	sid       string
	effect    string
	actions   []string
	condition string
}

func templatePolicyStatements(t *testing.T) []templatePolicyStatement {
	t.Helper()

	parsed := parseTemplate(t)
	properties := mappingValue(t, mappingValue(t, parsed.root, "Resources"), "ReaderRole")
	properties = mappingValue(t, properties, "Properties")
	policies := mappingValue(t, properties, "Policies")
	if policies == nil || policies.Kind != yaml.SequenceNode {
		t.Fatal("template has no inline Policies list")
	}

	var out []templatePolicyStatement
	for _, policy := range policies.Content {
		document := mappingValue(t, policy, "PolicyDocument")
		for _, statement := range statementList(t, document) {
			out = append(out, templatePolicyStatement{
				sid:       scalarOf(t, statement.node, "Sid"),
				effect:    scalarOf(t, statement.node, "Effect"),
				actions:   actionStrings(t, statement.node),
				condition: statement.condition,
			})
		}
	}
	return out
}

func TestCLITrustPolicyMatchesTemplate(t *testing.T) {
	parsed := parseTemplate(t)
	if parsed.trust == nil {
		t.Fatal("template has no assume-role trust statement")
	}

	policy := readCLIPolicy(t, "trust-policy.json")
	if policy.Version != "2012-10-17" {
		t.Errorf("CLI trust policy Version = %q, want 2012-10-17", policy.Version)
	}
	if len(policy.Statement) != 1 {
		t.Fatalf("CLI trust policy has %d statements, want exactly 1", len(policy.Statement))
	}

	statement := policy.Statement[0]
	if got, want := statement.Sid, scalarOf(t, parsed.trust, "Sid"); got != want {
		t.Errorf("CLI trust statement Sid = %q, want %q", got, want)
	}
	if got, want := statement.Effect, scalarOf(t, parsed.trust, "Effect"); got != want || got != "Allow" {
		t.Errorf("CLI trust statement Effect = %q, want %q", got, want)
	}
	if len(statement.Action) != 1 || statement.Action[0] != "sts:AssumeRole" {
		t.Errorf("CLI trust statement Action = %v, want [sts:AssumeRole]", statement.Action)
	}
	if statement.Resource != "" {
		t.Errorf("CLI trust statement has Resource %q; a trust policy names no resource", statement.Resource)
	}

	if len(statement.Principal) != 1 {
		t.Errorf("CLI trust Principal = %v, want only an AWS entry", statement.Principal)
	}
	principal := statement.Principal["AWS"]
	if principal != "__VERSUS_PRINCIPAL_ARN__" {
		t.Errorf("CLI trust principal = %q, want the __VERSUS_PRINCIPAL_ARN__ placeholder", principal)
	}
	if strings.ContainsAny(principal, "*?") {
		t.Errorf("CLI trust principal %q uses a wildcard", principal)
	}

	if len(statement.Condition) != 1 {
		t.Fatalf("CLI trust Condition = %v, want only StringEquals", statement.Condition)
	}
	stringEquals := statement.Condition["StringEquals"]
	if len(stringEquals) != 1 {
		t.Fatalf("CLI trust StringEquals = %v, want only sts:ExternalId", stringEquals)
	}
	if got := stringEquals["sts:ExternalId"]; got != "__EXTERNAL_ID__" {
		t.Errorf("CLI sts:ExternalId = %q, want the __EXTERNAL_ID__ placeholder", got)
	}

	// The template's operator and key must match the JSON exactly, so neither
	// artifact can loosen the condition (for example to StringLike).
	templateCondition := mappingValue(t, parsed.trust, "Condition")
	if templateCondition == nil {
		t.Fatal("template trust statement lacks a Condition block")
	}
	if len(templateCondition.Content) != 2 || templateCondition.Content[0].Value != "StringEquals" {
		t.Errorf("template trust condition operator is not StringEquals")
	}
	templateEquals := mappingValue(t, templateCondition, "StringEquals")
	if templateEquals == nil || len(templateEquals.Content) != 2 || templateEquals.Content[0].Value != "sts:ExternalId" {
		t.Errorf("template trust condition is not a single StringEquals sts:ExternalId match")
	}
}

// TestCLIPermissionsPolicyMatchesTemplate compares the JSON policy with the
// template statement by statement, including both deny statements. A missing
// billing deny, an added action or a changed effect fails here.
func TestCLIPermissionsPolicyMatchesTemplate(t *testing.T) {
	parsed := parseTemplate(t)
	policy := readCLIPolicy(t, "permissions-policy.json")
	if policy.Version != "2012-10-17" {
		t.Errorf("CLI permissions policy Version = %q, want 2012-10-17", policy.Version)
	}

	statements := map[string]cliStatement{}
	for _, statement := range policy.Statement {
		if statement.Sid == "" {
			t.Error("CLI permissions policy has a statement without a Sid")
		}
		if _, seen := statements[statement.Sid]; seen {
			t.Fatalf("CLI permissions policy declares statement %q twice", statement.Sid)
		}
		statements[statement.Sid] = statement
		if statement.Resource != "*" {
			t.Errorf("%s Resource = %q, want * as in the template", statement.Sid, statement.Resource)
		}
		if len(statement.Principal) != 0 {
			t.Errorf("%s carries a Principal; identity policies do not", statement.Sid)
		}
	}

	wantSIDs := map[string]bool{}
	for _, templateStatement := range templatePolicyStatements(t) {
		if templateStatement.condition != "" {
			// Optional data has its own file and is checked separately.
			continue
		}
		wantSIDs[templateStatement.sid] = true

		got, ok := statements[templateStatement.sid]
		if !ok {
			t.Errorf("CLI permissions policy lacks statement %q", templateStatement.sid)
			continue
		}
		if got.Effect != templateStatement.effect {
			t.Errorf("%s Effect = %q, want %q", templateStatement.sid, got.Effect, templateStatement.effect)
		}
		if missing, extra := diffSets(toSet(templateStatement.actions), toSet(got.Action)); len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s actions drifted: missing=%v extra=%v", templateStatement.sid, missing, extra)
		}
	}
	for sid := range statements {
		if !wantSIDs[sid] {
			t.Errorf("CLI permissions policy has statement %q that the template does not", sid)
		}
	}

	metadata, ok := statements["MetadataReads"]
	if !ok {
		t.Fatal("CLI permissions policy lacks the MetadataReads statement")
	}
	if missing, extra := diffSets(parsed.allow, toSet(metadata.Action)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("CLI allow list differs from the template: missing=%v extra=%v", missing, extra)
	}
	if missing, extra := diffSets(toSet(expectedAllowActions), toSet(metadata.Action)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("CLI allow list drifted from the catalog list: missing=%v extra=%v", missing, extra)
	}
	for _, action := range metadata.Action {
		if action == "*" || strings.HasSuffix(action, ":*") {
			t.Errorf("CLI allow entry %q is too broad; name the actions explicitly", action)
		}
	}

	// A deny that is silently dropped must fail even when the template still
	// has it, so the deny action sets are re-read here from the JSON alone.
	for _, group := range expectedDenyGroups {
		for _, action := range group.actions {
			if !policyHasAction(statements, "Deny", action) {
				t.Errorf("CLI permissions policy is missing deny %q (%s)", action, group.name)
			}
		}
	}

	optional := toSet(optionalActions)
	for _, action := range metadata.Action {
		if optional[action] {
			t.Errorf("optional action %q is allowed without opting in; it belongs in optional-data-policy.json", action)
		}
	}
}

func policyHasAction(statements map[string]cliStatement, effect, action string) bool {
	for _, statement := range statements {
		if statement.Effect != effect {
			continue
		}
		for _, candidate := range statement.Action {
			if candidate == action {
				return true
			}
		}
	}
	return false
}

func TestCLIOptionalDataPolicyMatchesTemplate(t *testing.T) {
	policy := readCLIPolicy(t, "optional-data-policy.json")
	if len(policy.Statement) != 1 {
		t.Fatalf("CLI optional-data policy has %d statements, want exactly 1", len(policy.Statement))
	}

	statement := policy.Statement[0]
	if statement.Sid != "OptionalDataReads" {
		t.Errorf("CLI optional statement Sid = %q, want OptionalDataReads", statement.Sid)
	}
	if statement.Effect != "Allow" {
		t.Errorf("CLI optional statement Effect = %q, want Allow", statement.Effect)
	}
	if statement.Resource != "*" {
		t.Errorf("CLI optional statement Resource = %q, want *", statement.Resource)
	}
	if missing, extra := diffSets(toSet(optionalActions), toSet(statement.Action)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("CLI optional allow list drifted: missing=%v extra=%v", missing, extra)
	}

	parsed := parseTemplate(t)
	gated := parsed.conditionalActions[optionalGate]
	if len(gated) == 0 {
		t.Fatalf("template has no %s allow actions", optionalGate)
	}
	if missing, extra := diffSets(gated, toSet(statement.Action)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("CLI optional actions differ from the template's %s block: missing=%v extra=%v", optionalGate, missing, extra)
	}
}

// TestCLIPoliciesUsePlaceholdersOnly fails when a real account ID, a concrete
// principal or a wildcard principal lands in a committed policy file.
func TestCLIPoliciesUsePlaceholdersOnly(t *testing.T) {
	for _, name := range []string{"trust-policy.json", "permissions-policy.json", "optional-data-policy.json"} {
		body := readArtifact(t, cliDir, name)
		if accountID.MatchString(body) {
			t.Errorf("%s contains a 12-digit account ID; committed policies must stay placeholders only", name)
		}
		if strings.Contains(body, "Root") || regexp.MustCompile(`:root"`).MatchString(body) {
			t.Errorf("%s names the account root; the principal must be a role ARN", name)
		}
	}

	trust := readCLIPolicy(t, "trust-policy.json")
	for _, want := range []string{"__VERSUS_PRINCIPAL_ARN__", "__EXTERNAL_ID__"} {
		body := readArtifact(t, cliDir, "trust-policy.json")
		if !strings.Contains(body, want) {
			t.Errorf("trust-policy.json lacks the %s placeholder", want)
		}
	}
	if len(trust.Statement) == 1 && strings.ContainsAny(trust.Statement[0].Principal["AWS"], "*?") {
		t.Error("trust-policy.json has a wildcard principal")
	}
}

var accountID = regexp.MustCompile(`(?:^|[^0-9])[0-9]{12}(?:[^0-9]|$)`)

// TestCreateScriptRefusesInvalidInput proves the validation runs before any
// AWS call: AWS_PROFILE points at a profile that does not exist, so only a
// local refusal can produce these messages.
func TestCreateScriptRefusesInvalidInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a POSIX sh script")
	}

	const validARN = "arn:aws:iam::123456789012:role/versus-app"
	const validExternalID = "versus-external-id-123"

	cases := []struct {
		name       string
		principal  string
		externalID string
		want       string
	}{
		{
			name:       "account root principal",
			principal:  "arn:aws:iam::123456789012:root",
			externalID: validExternalID,
			want:       "account root",
		},
		{
			name:       "short external id",
			principal:  validARN,
			externalID: "too-short",
			want:       "at least 16",
		},
		{
			name:       "non-role principal",
			principal:  "arn:aws:iam::123456789012:user/versus",
			externalID: validExternalID,
			want:       "exact IAM role ARN",
		},
		{
			name:       "external id with a space",
			principal:  validARN,
			externalID: "versus external id 123",
			want:       "printable ASCII",
		},
		{
			name:       "wildcard principal",
			principal:  "arn:aws:iam::123456789012:role/versus-*",
			externalID: validExternalID,
			want:       "exact IAM role ARN",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cmd := exec.Command("sh", filepath.Join(cliDir, "create-versus-reader.sh"), testCase.principal, testCase.externalID)
			cmd.Env = append(os.Environ(), "AWS_PROFILE=versus-profile-that-does-not-exist")

			output, err := cmd.CombinedOutput()
			text := string(output)
			if err == nil {
				t.Fatalf("script accepted %q; it must refuse before calling AWS", testCase.principal)
			}
			if !strings.HasPrefix(text, "error: ") {
				t.Errorf("output = %q, want a validation error before any AWS call", text)
			}
			if !strings.Contains(text, testCase.want) {
				t.Errorf("error = %q, want it to mention %q", text, testCase.want)
			}
			if strings.Contains(text, "caller identity") {
				t.Errorf("script reached the AWS caller-identity check before validating its input: %q", text)
			}
			if strings.Contains(text, testCase.externalID) {
				t.Errorf("script echoed the External ID: %q", text)
			}
		})
	}
}
