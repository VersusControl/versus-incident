package awsdeploy

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
)

const (
	templateFile     = "versus-reader.yaml"
	terraformDir     = "terraform/versus-reader"
	costTemplateFile = "versus-cost-reader.yaml"
	costTerraformDir = "terraform/versus-cost-reader"
	optionalGate     = "IncludeOptionalData"
	cliDir           = "cli"
)

// roleArtifact names one role and the two artifacts that create it.
type roleArtifact struct {
	name         string
	template     string
	resource     string
	terraformDir string
	variables    []string
}

var (
	readerArtifact = roleArtifact{
		name:         "versus-reader",
		template:     templateFile,
		resource:     "ReaderRole",
		terraformDir: terraformDir,
		variables:    []string{"versus_principal_arn", "external_id", "optional_data", "tags"},
	}
	costReaderArtifact = roleArtifact{
		name:         "versus-cost-reader",
		template:     costTemplateFile,
		resource:     "CostReaderRole",
		terraformDir: costTerraformDir,
		variables:    []string{"versus_principal_arn", "external_id", "tags"},
	}
	roleArtifacts = []roleArtifact{readerArtifact, costReaderArtifact}
)

// roleOnlyActions are base reads the role grants that no catalog entry,
// collector fix list or collector call names: bucket location and Resource
// Explorer search from the onboarding plan.
var roleOnlyActions = []string{
	"s3:GetBucketLocation",
	"resource-explorer-2:Search",
}

// fixActionSet is every fix action in the code: the service catalog entries
// and the collector units outside the catalog (fixActions in pkg/aws/collect).
func fixActionSet(t *testing.T) map[string]bool {
	t.Helper()

	set := map[string]bool{}
	for _, service := range catalog.Services() {
		for _, action := range service.FixActions {
			set[action] = true
		}
	}
	for _, action := range collectorFixActions(t) {
		set[action] = true
	}
	return set
}

// expectedAllowActions is the metadata-only allow list. It is derived, not
// written out: every fix action in the code plus roleOnlyActions.
func expectedAllowActions(t *testing.T) []string {
	t.Helper()

	set := fixActionSet(t)
	for _, action := range roleOnlyActions {
		set[action] = true
	}
	return keys(set)
}

// costReadActions are the cost reads the FinOps fetchers call, derived from
// the requests pkg/aws/cost builds (costFetcherActions in cost_calls_test.go).
// versus-reader grants them only behind OptionalData; versus-cost-reader
// grants them and nothing else. Both roles must name each action exactly, so
// no wildcard grants an operation the code does not call.
func costReadActions(t *testing.T) []string {
	t.Helper()

	actions := keys(costFetcherActions(t))
	if len(actions) == 0 {
		t.Fatal("found no cost fetcher requests in pkg/aws/cost")
	}
	return actions
}

// costServices are the only service prefixes versus-cost-reader may allow.
var costServices = map[string]bool{
	"ce":                    true,
	"budgets":               true,
	"cost-optimization-hub": true,
	"compute-optimizer":     true,
}

// postureActions are the non-cost reads behind OptionalData.
var postureActions = []string{
	"securityhub:GetFindings",
	"config:SelectResourceConfig",
	"trustedadvisor:Describe*",
}

// optionalActions are added only when the OptionalData parameter is on: the
// cost reads plus posture and check reads.
func optionalActions(t *testing.T) []string {
	t.Helper()
	return concat(costReadActions(t), postureActions)
}

func concat(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		out = append(out, list...)
	}
	return out
}

type denyGroup struct {
	name    string
	actions []string
}

// billingDenyActions is the billing-surface deny group: billing account
// details, invoices, payment methods, tax registrations, purchase orders,
// consolidated billing and account settings. It is modelled separately
// because it is a distinct statement in every artifact of both roles.
var billingDenyActions = []string{
	"account:*",
	"aws-portal:*",
	"billing:*",
	"consolidatedbilling:*",
	"invoicing:*",
	"payments:*",
	"purchase-orders:*",
	"tax:*",
}

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

	if missing, extra := diffSets(toSet(expectedAllowActions(t)), parsed.allow); len(missing) > 0 || len(extra) > 0 {
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
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) { checkTrustPolicy(t, artifact) })
	}
}

func checkTrustPolicy(t *testing.T, artifact roleArtifact) {
	parsed := parseRoleTemplate(t, artifact)

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

	mainTF := readArtifact(t, artifact.terraformDir, "main.tf")
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
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) { checkPrincipalArnPattern(t, artifact) })
	}
}

func checkPrincipalArnPattern(t *testing.T, artifact roleArtifact) {
	parsed := parseRoleTemplate(t, artifact)

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

	variables := readArtifact(t, artifact.terraformDir, "variables.tf")
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
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) { checkPrincipalArnValidation(t, artifact) })
	}
}

func checkPrincipalArnValidation(t *testing.T, artifact roleArtifact) {
	variables := readArtifact(t, artifact.terraformDir, "variables.tf")
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
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) { checkExternalIDLength(t, artifact) })
	}
}

func checkExternalIDLength(t *testing.T, artifact roleArtifact) {
	parsed := parseRoleTemplate(t, artifact)

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

	variables := readArtifact(t, artifact.terraformDir, "variables.tf")
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
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) {
			parsed := parseRoleTemplate(t, artifact)
			params := mappingValue(t, parsed.root, "Parameters")
			principalParam := mappingValue(t, params, "VersusPrincipalArn")
			principalDesc := strings.ToLower(scalarOf(t, principalParam, "Description"))

			variables := readArtifact(t, artifact.terraformDir, "variables.tf")
			tfDesc := strings.ToLower(terraformAttribute(t, blockBody(t, variables, `variable "versus_principal_arn"`), "description"))

			if !warnsAgainstRoot(principalDesc) {
				t.Errorf("VersusPrincipalArn description must say not to use the account root, got %q", principalDesc)
			}
			if !warnsAgainstRoot(tfDesc) {
				t.Errorf("versus_principal_arn description must say not to use the account root, got %q", tfDesc)
			}
			if !strings.Contains(principalDesc, "role") || !strings.Contains(tfDesc, "role") {
				t.Error("both principal descriptions must say the value is a Versus role ARN")
			}
		})
	}

	readme := readArtifact(t, "README.md")

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

// terraformStatement is one statement block of a Terraform policy document.
type terraformStatement struct {
	sid        string
	effect     string
	actions    []string
	notActions []string
	resources  []string
}

// terraformDenyStatements returns the Deny statements of the versus-reader
// deny policy so each group, including the billing group, is checked on its own.
func terraformDenyStatements(t *testing.T, mainTF string) []terraformStatement {
	t.Helper()

	var out []terraformStatement
	for _, statement := range terraformStatements(t, mainTF, "deny_reads") {
		if len(statement.notActions) > 0 {
			t.Errorf("versus-reader deny statement %q uses not_actions", statement.sid)
		}
		if statement.effect == "Deny" {
			out = append(out, statement)
		}
	}
	return out
}

var (
	terraformActionsList    = regexp.MustCompile(`(?ms)^\s*actions\s*=\s*\[(.*?)\]`)
	terraformNotActionsList = regexp.MustCompile(`(?ms)^\s*not_actions\s*=\s*\[(.*?)\]`)
	terraformResourcesList  = regexp.MustCompile(`(?ms)^\s*resources\s*=\s*\[(.*?)\]`)
	terraformPolicyDocument = regexp.MustCompile(`data "aws_iam_policy_document" "([a-z_]+)"`)
	quotedString            = regexp.MustCompile(`"([^"]*)"`)
)

// terraformStatements splits one aws_iam_policy_document into its statement
// blocks and reads actions and not_actions separately, so a deny-all-except
// list is never mistaken for a deny list.
func terraformStatements(t *testing.T, mainTF, document string) []terraformStatement {
	t.Helper()

	body := stripHCLComments(blockBody(t, mainTF, fmt.Sprintf(`data "aws_iam_policy_document" %q`, document)))
	var out []terraformStatement
	for offset := 0; ; {
		rel := strings.Index(body[offset:], "statement")
		if rel < 0 {
			return out
		}
		start := offset + rel
		block := blockBody(t, body[start:], "statement")
		out = append(out, terraformStatement{
			sid:        terraformAttribute(t, block, "sid"),
			effect:     terraformAttribute(t, block, "effect"),
			actions:    terraformList(terraformActionsList, block),
			notActions: terraformList(terraformNotActionsList, block),
			resources:  terraformList(terraformResourcesList, block),
		})
		offset = start + len("statement")
	}
}

func terraformList(attribute *regexp.Regexp, block string) []string {
	match := attribute.FindStringSubmatch(block)
	if match == nil {
		return nil
	}
	var out []string
	for _, item := range quotedString.FindAllStringSubmatch(match[1], -1) {
		out = append(out, item[1])
	}
	return out
}

// terraformPolicyDocuments names every aws_iam_policy_document in a module.
func terraformPolicyDocuments(mainTF string) []string {
	var out []string
	for _, match := range terraformPolicyDocument.FindAllStringSubmatch(mainTF, -1) {
		out = append(out, match[1])
	}
	return out
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
	if missing, extra := diffSets(toSet(optionalActions(t)), got); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("optional allow list drifted: missing=%v extra=%v", missing, extra)
	}
	for _, action := range optionalActions(t) {
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
	if missing, extra := diffSets(toSet(optionalActions(t)), toSet(quotedActions(t, optionalTF))); len(missing) > 0 || len(extra) > 0 {
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
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) {
			variables := readArtifact(t, artifact.terraformDir, "variables.tf")
			declared := regexp.MustCompile(`(?m)^variable "([a-z_]+)"`).FindAllStringSubmatch(variables, -1)
			got := make([]string, 0, len(declared))
			for _, match := range declared {
				got = append(got, match[1])
			}
			if !sameSet(got, artifact.variables) {
				t.Errorf("Terraform variables = %v, want %v", got, artifact.variables)
			}
			if !regexp.MustCompile(`(?m)^\s*sensitive\s*=\s*true\s*$`).MatchString(blockBody(t, variables, `variable "external_id"`)) {
				t.Error("Terraform external_id must be marked sensitive = true")
			}

			outputs := readArtifact(t, artifact.terraformDir, "outputs.tf")
			blockBody(t, outputs, `output "role_arn"`)
			blockBody(t, outputs, `output "role_name"`)
		})
	}
}

func TestTemplateResourcesAndOutputsAreDocumented(t *testing.T) {
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) { checkResourcesAndOutputsDocumented(t, artifact) })
	}
}

func checkResourcesAndOutputsDocumented(t *testing.T, artifact roleArtifact) {
	parsed := parseRoleTemplate(t, artifact)

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
	// denyExcept holds the NotAction list of a Deny statement: every action
	// outside it is denied. Only versus-cost-reader uses one.
	denyExcept           map[string]bool
	denyExceptStatements int
	trust                *yaml.Node
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
	return parseRoleTemplate(t, readerArtifact)
}

func parseRoleTemplate(t *testing.T, artifact roleArtifact) templatePolicy {
	t.Helper()

	data, err := os.ReadFile(artifact.template)
	if err != nil {
		t.Fatalf("read %s: %v", artifact.template, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", artifact.template, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("%s is empty", artifact.template)
	}

	parsed := templatePolicy{
		root:               doc.Content[0],
		allow:              map[string]bool{},
		conditionalActions: map[string]map[string]bool{},
		deny:               map[string]bool{},
		denyExcept:         map[string]bool{},
	}

	resources := mappingValue(t, parsed.root, "Resources")
	if resources == nil || len(resources.Content) != 2 {
		t.Fatalf("%s must create exactly one resource, the %s role", artifact.template, artifact.resource)
	}
	role := mappingValue(t, resources, artifact.resource)
	if role == nil {
		t.Fatalf("%s has no %s resource", artifact.template, artifact.resource)
	}
	if got := scalarOf(t, role, "Type"); got != "AWS::IAM::Role" {
		t.Errorf("%s Type = %q, want AWS::IAM::Role", artifact.resource, got)
	}
	properties := mappingValue(t, role, "Properties")
	if got := scalarOf(t, properties, "RoleName"); got != artifact.name {
		t.Errorf("%s RoleName = %q, want %q", artifact.resource, got, artifact.name)
	}
	for _, key := range []string{"ManagedPolicyArns", "PermissionsBoundary"} {
		if mappingValue(t, properties, key) != nil {
			t.Errorf("%s sets %s; every permission must be in the linted inline policies", artifact.resource, key)
		}
	}

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
			sid := scalarOf(t, statement.node, "Sid")
			if mappingValue(t, statement.node, "Condition") != nil {
				t.Errorf("statement %q has a Condition; allows and denies here must be unconditional", sid)
			}
			if got := scalarOf(t, statement.node, "Resource"); got != "*" {
				t.Errorf("statement %q Resource = %q, want *", sid, got)
			}
			actions := actionStrings(t, statement.node)
			notActions := notActionStrings(t, statement.node)
			switch {
			case len(actions) > 0 && len(notActions) > 0:
				t.Errorf("statement %q sets both Action and NotAction", sid)
			case len(actions) == 0 && len(notActions) == 0:
				t.Errorf("statement %q names no action", sid)
			case len(notActions) > 0 && effect != "Deny":
				t.Errorf("statement %q uses NotAction with Effect %q; an allow with NotAction grants every other action", sid, effect)
			case len(notActions) > 0 && statement.condition != "":
				t.Errorf("statement %q is a NotAction deny gated by %q; it must always apply", sid, statement.condition)
			case len(notActions) > 0:
				parsed.denyExceptStatements++
				for _, action := range notActions {
					parsed.denyExcept[action] = true
				}
			}
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
	return actionList(t, mappingValue(t, statement, "Action"))
}

func notActionStrings(t *testing.T, statement *yaml.Node) []string {
	t.Helper()
	return actionList(t, mappingValue(t, statement, "NotAction"))
}

func actionList(t *testing.T, action *yaml.Node) []string {
	t.Helper()

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
	sid        string
	effect     string
	actions    []string
	notActions []string
	condition  string
}

func templatePolicyStatements(t *testing.T) []templatePolicyStatement {
	t.Helper()
	return roleTemplatePolicyStatements(t, readerArtifact)
}

func roleTemplatePolicyStatements(t *testing.T, artifact roleArtifact) []templatePolicyStatement {
	t.Helper()

	parsed := parseRoleTemplate(t, artifact)
	properties := mappingValue(t, mappingValue(t, parsed.root, "Resources"), artifact.resource)
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
				sid:        scalarOf(t, statement.node, "Sid"),
				effect:     scalarOf(t, statement.node, "Effect"),
				actions:    actionStrings(t, statement.node),
				notActions: notActionStrings(t, statement.node),
				condition:  statement.condition,
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
	if missing, extra := diffSets(toSet(expectedAllowActions(t)), toSet(metadata.Action)); len(missing) > 0 || len(extra) > 0 {
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

	optional := toSet(optionalActions(t))
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
	if missing, extra := diffSets(toSet(optionalActions(t)), toSet(statement.Action)); len(missing) > 0 || len(extra) > 0 {
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

// readVerbs are the only operation prefixes an allow may use. Each one names a
// read in IAM; a write verb such as Put, Create, Update, Delete, Modify, Run,
// Start or Invoke matches none of them.
var readVerbs = []string{"Describe", "Get", "List", "Lookup", "Search", "Select", "View"}

// isReadAction reports whether an IAM action names only reads. apigateway:GET
// is the HTTP-verb form API Gateway uses for its read calls.
func isReadAction(action string) bool {
	if action == "apigateway:GET" {
		return true
	}
	service, operation, ok := strings.Cut(action, ":")
	if !ok || service == "" || service == "*" || strings.Contains(service, "*") {
		return false
	}
	for _, verb := range readVerbs {
		if strings.HasPrefix(operation, verb) {
			return true
		}
	}
	return false
}

func TestIsReadActionRejectsWrites(t *testing.T) {
	for _, action := range []string{
		"ce:GetCostAndUsage", "budgets:ViewBudget", "compute-optimizer:Get*Recommendations",
		"cost-optimization-hub:ListRecommendations", "ec2:Describe*", "config:SelectResourceConfig",
		"apigateway:GET", "cloudtrail:LookupEvents", "resource-explorer-2:Search",
	} {
		if !isReadAction(action) {
			t.Errorf("isReadAction(%q) = false, want true", action)
		}
	}
	for _, action := range []string{
		"*", "ce:*", "*:GetCostAndUsage", "ce:CreateCostCategoryDefinition",
		"ce:UpdateCostAllocationTagsStatus", "budgets:ModifyBudget", "budgets:ExecuteBudgetAction",
		"compute-optimizer:PutRecommendationPreferences", "compute-optimizer:UpdateEnrollmentStatus",
		"cost-optimization-hub:UpdatePreferences", "apigateway:POST", "ec2:RunInstances",
		"s3:PutObject", "iam:PassRole", "lambda:InvokeFunction", "ce:getCostAndUsage",
	} {
		if isReadAction(action) {
			t.Errorf("isReadAction(%q) = true, want false", action)
		}
	}
}

// TestNoWriteActionIsAllowed reads every allow statement in both roles, in the
// CloudFormation templates, the Terraform modules and the CLI policy files,
// and fails on any action that is not a read.
func TestNoWriteActionIsAllowed(t *testing.T) {
	for _, artifact := range roleArtifacts {
		t.Run(artifact.name, func(t *testing.T) {
			parsed := parseRoleTemplate(t, artifact)
			for action := range parsed.allow {
				if !isReadAction(action) {
					t.Errorf("%s allows %q, which is not a read", artifact.template, action)
				}
			}
			for condition, actions := range parsed.conditionalActions {
				for action := range actions {
					if !isReadAction(action) {
						t.Errorf("%s allows %q behind %s, which is not a read", artifact.template, action, condition)
					}
				}
			}

			mainTF := readArtifact(t, artifact.terraformDir, "main.tf")
			if strings.Contains(mainTF, "aws_iam_role_policy_attachment") || strings.Contains(mainTF, "managed_policy_arns") {
				t.Errorf("%s attaches a managed policy; every permission must be in the linted inline policies", artifact.terraformDir)
			}
			for _, document := range terraformPolicyDocuments(mainTF) {
				if document == "assume_role" {
					continue
				}
				for _, statement := range terraformStatements(t, mainTF, document) {
					if statement.effect != "Allow" && statement.effect != "Deny" {
						t.Errorf("%s/%s statement %q effect = %q; state Allow or Deny explicitly", artifact.terraformDir, document, statement.sid, statement.effect)
					}
					if !sameSet(statement.resources, []string{"*"}) {
						t.Errorf("%s/%s statement %q resources = %v, want [*]", artifact.terraformDir, document, statement.sid, statement.resources)
					}
					if statement.effect != "Allow" {
						continue
					}
					if len(statement.notActions) > 0 {
						t.Errorf("%s/%s statement %q allows with not_actions, which grants every other action", artifact.terraformDir, document, statement.sid)
					}
					for _, action := range statement.actions {
						if !isReadAction(action) {
							t.Errorf("%s/%s allows %q, which is not a read", artifact.terraformDir, document, action)
						}
					}
				}
			}
		})
	}

	t.Run("cli", func(t *testing.T) {
		for _, name := range []string{"permissions-policy.json", "optional-data-policy.json"} {
			for _, statement := range readCLIPolicy(t, name).Statement {
				if statement.Effect != "Allow" {
					continue
				}
				for _, action := range statement.Action {
					if !isReadAction(action) {
						t.Errorf("%s allows %q, which is not a read", name, action)
					}
				}
			}
		}
	})
}

// TestReaderOptionalCostReadsMatchCostReader keeps the two roles on one cost
// list: the opt-in cost reads of versus-reader and the whole allow list of
// versus-cost-reader are the same actions.
func TestReaderOptionalCostReadsMatchCostReader(t *testing.T) {
	reader := parseRoleTemplate(t, readerArtifact)
	costReader := parseRoleTemplate(t, costReaderArtifact)

	readerCost := map[string]bool{}
	for action := range reader.conditionalActions[optionalGate] {
		service, _, _ := strings.Cut(action, ":")
		if costServices[service] {
			readerCost[action] = true
		}
	}
	for action := range reader.allow {
		service, _, _ := strings.Cut(action, ":")
		if costServices[service] {
			t.Errorf("versus-reader allows cost read %q without OptionalData", action)
		}
	}
	if missing, extra := diffSets(toSet(costReadActions(t)), readerCost); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("versus-reader optional cost reads drifted from the cost list: missing=%v extra=%v", missing, extra)
	}
	if missing, extra := diffSets(readerCost, costReader.allow); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("versus-cost-reader allow list differs from versus-reader's optional cost reads: missing=%v extra=%v", missing, extra)
	}
}

// TestCostReaderAllowsOnlyCostReads fails when versus-cost-reader gains any
// action outside the cost list, an inventory read, an opt-in parameter or a
// conditional statement, in either artifact.
func TestCostReaderAllowsOnlyCostReads(t *testing.T) {
	parsed := parseRoleTemplate(t, costReaderArtifact)

	if missing, extra := diffSets(toSet(costReadActions(t)), parsed.allow); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("versus-cost-reader allow list drifted from the cost list: missing=%v extra=%v", missing, extra)
	}
	if len(parsed.conditionalActions) > 0 {
		t.Errorf("versus-cost-reader has conditional allows %v; the cost-only role has no opt-in", keys(parsed.conditionalAll()))
	}
	if conditions := mappingValue(t, parsed.root, "Conditions"); conditions != nil {
		t.Error("versus-cost-reader declares Conditions; the cost-only role has no opt-in")
	}
	params := mappingValue(t, parsed.root, "Parameters")
	for i := 0; i+1 < len(params.Content); i += 2 {
		if name := params.Content[i].Value; name != "VersusPrincipalArn" && name != "ExternalId" {
			t.Errorf("versus-cost-reader has parameter %q; only VersusPrincipalArn and ExternalId are allowed", name)
		}
	}

	inventory := toSet(expectedAllowActions(t))
	for action := range parsed.allow {
		service, _, _ := strings.Cut(action, ":")
		if !costServices[service] {
			t.Errorf("versus-cost-reader allows %q outside the cost services", action)
		}
		if inventory[action] {
			t.Errorf("versus-cost-reader allows inventory read %q", action)
		}
	}

	mainTF := readArtifact(t, costTerraformDir, "main.tf")
	if got := terraformPolicyDocuments(mainTF); !sameSet(got, []string{"assume_role", "cost_reads", "deny_reads"}) {
		t.Errorf("versus-cost-reader Terraform policy documents = %v, want assume_role, cost_reads, deny_reads", got)
	}
	statements := terraformStatements(t, mainTF, "cost_reads")
	if len(statements) != 1 {
		t.Fatalf("Terraform cost_reads has %d statements, want 1", len(statements))
	}
	allow := statements[0]
	if allow.sid != "CostReads" || allow.effect != "Allow" || len(allow.notActions) > 0 {
		t.Errorf("Terraform cost_reads statement = %+v, want sid CostReads, effect Allow, actions only", allow)
	}
	if missing, extra := diffSets(parsed.allow, toSet(allow.actions)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("Terraform cost_reads differs from the template: missing=%v extra=%v", missing, extra)
	}
	for _, header := range []string{`resource "aws_iam_role" "versus_cost_reader"`, `resource "aws_iam_role_policy" "cost_reads"`, `resource "aws_iam_role_policy" "deny_reads"`} {
		blockBody(t, mainTF, header)
	}
	if regexp.MustCompile(`(?m)^\s*name\s*=\s*"versus-cost-reader"\s*$`).FindString(blockBody(t, mainTF, `resource "aws_iam_role" "versus_cost_reader"`)) == "" {
		t.Error("Terraform role name must be versus-cost-reader")
	}
	if regexp.MustCompile(`(?m)^\s*(count|for_each)\s*=`).MatchString(mainTF) {
		t.Error("versus-cost-reader Terraform policies must always be attached, with no count or for_each")
	}
}

// TestCostReaderDeniesEverythingElse checks the deny-all-except statement and
// the billing denies in both artifacts. The NotAction list must equal the
// cost list exactly: one extra entry would leave that action undenied.
func TestCostReaderDeniesEverythingElse(t *testing.T) {
	parsed := parseRoleTemplate(t, costReaderArtifact)

	if parsed.denyExceptStatements != 1 {
		t.Errorf("versus-cost-reader has %d NotAction deny statements, want 1", parsed.denyExceptStatements)
	}
	if missing, extra := diffSets(toSet(costReadActions(t)), parsed.denyExcept); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("versus-cost-reader deny-all-except list drifted from the cost list: missing=%v extra=%v", missing, extra)
	}
	for action := range parsed.deny {
		if parsed.allow[action] {
			t.Errorf("action %q is both allowed and denied", action)
		}
	}

	statements := map[string]templatePolicyStatement{}
	for _, statement := range roleTemplatePolicyStatements(t, costReaderArtifact) {
		statements[statement.sid] = statement
	}
	if got := statements["DenyBillingAccountSurfaces"]; got.effect != "Deny" || !sameSet(got.actions, billingDenyActions) {
		t.Errorf("versus-cost-reader DenyBillingAccountSurfaces = %+v, want Deny %v", got, billingDenyActions)
	}
	if got := statements["DenyEverythingButCostReads"]; got.effect != "Deny" || len(got.actions) > 0 || !sameSet(got.notActions, costReadActions(t)) {
		t.Errorf("versus-cost-reader DenyEverythingButCostReads = %+v, want Deny with NotAction %v", got, costReadActions(t))
	}

	mainTF := readArtifact(t, costTerraformDir, "main.tf")
	tfDeny := map[string]bool{}
	bySID := map[string]terraformStatement{}
	for _, statement := range terraformStatements(t, mainTF, "deny_reads") {
		if statement.effect != "Deny" {
			t.Errorf("Terraform deny_reads statement %q effect = %q, want Deny", statement.sid, statement.effect)
		}
		if _, seen := bySID[statement.sid]; seen {
			t.Errorf("Terraform deny statement sid %q is declared twice", statement.sid)
		}
		bySID[statement.sid] = statement
		for _, action := range statement.actions {
			tfDeny[action] = true
		}
	}
	if len(bySID) != len(statements)-1 {
		t.Errorf("Terraform deny_reads has %d statements, the template has %d deny statements", len(bySID), len(statements)-1)
	}
	except, ok := bySID["DenyEverythingButCostReads"]
	if !ok {
		t.Fatal("Terraform deny_reads has no DenyEverythingButCostReads statement; the role would not be cost-only")
	}
	if len(except.actions) > 0 {
		t.Errorf("Terraform DenyEverythingButCostReads must use not_actions only, got actions %v", except.actions)
	}
	if missing, extra := diffSets(parsed.denyExcept, toSet(except.notActions)); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("Terraform not_actions differ from the template: missing=%v extra=%v", missing, extra)
	}
	if billing := bySID["DenyBillingAccountSurfaces"]; !sameSet(billing.actions, billingDenyActions) {
		t.Errorf("Terraform DenyBillingAccountSurfaces = %v, want %v", billing.actions, billingDenyActions)
	}
	if missing, extra := diffSets(parsed.deny, tfDeny); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("Terraform and CloudFormation deny sets are not identical: missing from Terraform=%v extra in Terraform=%v", missing, extra)
	}
}

// collectDir holds the AWS collectors whose calls the reader role must allow.
var collectDir = filepath.Join("..", "..", "pkg", "aws", "collect")

const sdkServicePrefix = "github.com/aws/aws-sdk-go-v2/service/"

// sdkToIAMService maps an AWS SDK service package to its IAM prefix where the
// two differ.
var sdkToIAMService = map[string]string{
	"cloudwatchlogs":            "logs",
	"configservice":             "config",
	"elasticloadbalancingv2":    "elasticloadbalancing",
	"eventbridge":               "events",
	"resourcegroupstaggingapi":  "tag",
	"servicecatalogappregistry": "servicecatalog",
	"sfn":                       "states",
}

// sdkToIAMAction maps the SDK operations whose IAM action has another name.
// API Gateway authorises every read as apigateway:GET.
var sdkToIAMAction = map[string]string{
	"s3:GetBucketEncryption":  "s3:GetEncryptionConfiguration",
	"s3:GetPublicAccessBlock": "s3:GetBucketPublicAccessBlock",
	"s3:ListBuckets":          "s3:ListAllMyBuckets",
}

func iamAction(sdkService, operation string) string {
	service := sdkService
	if mapped, ok := sdkToIAMService[sdkService]; ok {
		service = mapped
	}
	if service == "apigateway" && strings.HasPrefix(operation, "Get") {
		return "apigateway:GET"
	}
	action := service + ":" + operation
	if mapped, ok := sdkToIAMAction[action]; ok {
		return mapped
	}
	return action
}

func parseCollectorFiles(t *testing.T) []*ast.File {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(collectDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	positions := token.NewFileSet()
	var out []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, file)
	}
	if len(out) == 0 {
		t.Fatalf("no collector sources under %s", collectDir)
	}
	return out
}

// sdkImports maps each import name in a file to its AWS SDK service package.
func sdkImports(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, `"`)
		if !strings.HasPrefix(path, sdkServicePrefix) {
			continue
		}
		service := strings.TrimPrefix(path, sdkServicePrefix)
		if strings.Contains(service, "/") {
			continue
		}
		name := service
		if imported.Name != nil {
			name = imported.Name.Name
		}
		out[name] = service
	}
	return out
}

// sdkClientType returns the SDK service of a *service.Client type expression.
func sdkClientType(expr ast.Expr, imports map[string]string) string {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return ""
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Client" {
		return ""
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return imports[pkg.Name]
}

// collectorActions reads the collector sources and returns every AWS SDK
// operation they call, in IAM notation. A client is a variable assigned from
// <service>.NewFromConfig, from a helper that returns *<service>.Client, or a
// parameter of that type; paginators count as their operation.
func collectorActions(t *testing.T) map[string]bool {
	t.Helper()

	files := parseCollectorFiles(t)
	helpers := map[string]string{}
	for _, file := range files {
		imports := sdkImports(file)
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Type.Results == nil {
				continue
			}
			for _, result := range function.Type.Results.List {
				if service := sdkClientType(result.Type, imports); service != "" {
					helpers[function.Name.Name] = service
				}
			}
		}
	}

	actions := map[string]bool{}
	for _, file := range files {
		imports := sdkImports(file)
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			clients := map[string]string{}
			for _, field := range function.Type.Params.List {
				if service := sdkClientType(field.Type, imports); service != "" {
					for _, name := range field.Names {
						clients[name.Name] = service
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.AssignStmt:
					if len(node.Rhs) != 1 || len(node.Lhs) == 0 {
						return true
					}
					target, ok := node.Lhs[0].(*ast.Ident)
					if !ok {
						return true
					}
					delete(clients, target.Name)
					call, ok := node.Rhs[0].(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						if pkg, ok := fun.X.(*ast.Ident); ok && fun.Sel.Name == "NewFromConfig" && imports[pkg.Name] != "" {
							clients[target.Name] = imports[pkg.Name]
						}
					case *ast.Ident:
						if service, ok := helpers[fun.Name]; ok {
							clients[target.Name] = service
						}
					}
				case *ast.CallExpr:
					selector, ok := node.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					receiver, ok := selector.X.(*ast.Ident)
					if !ok {
						return true
					}
					if service, ok := clients[receiver.Name]; ok && selector.Sel.Name != "Options" {
						actions[iamAction(service, selector.Sel.Name)] = true
					}
					if service := imports[receiver.Name]; service != "" && strings.HasPrefix(selector.Sel.Name, "New") && strings.HasSuffix(selector.Sel.Name, "Paginator") {
						operation := strings.TrimSuffix(strings.TrimPrefix(selector.Sel.Name, "New"), "Paginator")
						actions[iamAction(service, operation)] = true
					}
				}
				return true
			})
		}
	}
	return actions
}

// collectorFixActions returns the IAM actions named in fixActions in the
// collector package: the fix text for collector units outside the catalog.
func collectorFixActions(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, file := range parseCollectorFiles(t) {
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Name.Name != "fixActions" || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if ok && literal.Kind == token.STRING {
					if value := strings.Trim(literal.Value, "`\""); strings.Contains(value, ":") {
						out = append(out, value)
					}
				}
				return true
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("found no fixActions IAM actions in the collector package")
	}
	return out
}

// iamMatch reports whether an IAM action pattern with * wildcards matches an
// action. IAM compares action names case-insensitively.
func iamMatch(pattern, action string) bool {
	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.ToLower(pattern)), `\*`, ".*") + "$"
	return regexp.MustCompile(expr).MatchString(strings.ToLower(action))
}

func allowedBy(patterns map[string]bool, action string) bool {
	for pattern := range patterns {
		if iamMatch(pattern, action) {
			return true
		}
	}
	return false
}

// TestReaderAllowsEveryCollectorAndProbeCall cross-checks the role against the
// code: every operation a collector calls and every catalog probe operation
// must be granted by the base (not opt-in) allow list in the template, the
// Terraform module and the CLI policy, and none may be denied.
func TestReaderAllowsEveryCollectorAndProbeCall(t *testing.T) {
	required := collectorActions(t)
	for _, expected := range []string{"ec2:DescribeInstances", "cloudtrail:LookupEvents", "sqs:GetQueueAttributes", "s3:GetEncryptionConfiguration", "apigateway:GET"} {
		if !required[expected] {
			t.Errorf("collector scan did not see %s; the scan is broken", expected)
		}
	}
	if len(required) < 40 {
		t.Fatalf("collector scan saw %d distinct actions; expected the whole collector set", len(required))
	}
	t.Logf("collectors call %d distinct actions: %v", len(required), keys(required))
	// Catalog probe operations are usually bare operation names; the IAM
	// prefix is the one the entry's own fix actions use.
	for _, service := range catalog.Services() {
		prefix, operation, qualified := strings.Cut(service.Operation, ":")
		if !qualified {
			operation = service.Operation
			if len(service.FixActions) == 0 {
				t.Errorf("catalog entry %q has no fix actions to take an IAM prefix from", service.ID)
				continue
			}
			prefix, _, _ = strings.Cut(service.FixActions[0], ":")
		}
		required[iamAction(prefix, operation)] = true
	}

	parsed := parseTemplate(t)
	mainTF := readArtifact(t, terraformDir, "main.tf")
	sources := map[string]map[string]bool{
		templateFile:                     parsed.allow,
		terraformDir + "/metadata_reads": toSet(quotedActions(t, blockBody(t, mainTF, `data "aws_iam_policy_document" "metadata_reads"`))),
	}
	for _, statement := range readCLIPolicy(t, "permissions-policy.json").Statement {
		if statement.Sid == "MetadataReads" {
			sources["cli/permissions-policy.json"] = toSet(statement.Action)
		}
	}
	fixes := fixActionSet(t)
	for _, action := range keys(required) {
		if !allowedBy(fixes, action) {
			t.Errorf("%s is called by the collectors or probes but no catalog or collector fix action covers it", action)
		}
		for source, allow := range sources {
			if !allowedBy(allow, action) {
				t.Errorf("%s does not allow %s, which the collectors or probes call", source, action)
			}
		}
		if allowedBy(parsed.deny, action) {
			t.Errorf("%s is called by the collectors or probes but explicitly denied", action)
		}
	}
}

func TestIAMMatch(t *testing.T) {
	for _, testCase := range []struct {
		pattern, action string
		want            bool
	}{
		{"ec2:Describe*", "ec2:DescribeInstances", true},
		{"cloudwatch:DescribeAlarms*", "cloudwatch:DescribeAlarmsForMetric", true},
		{"compute-optimizer:Get*Recommendations", "compute-optimizer:GetEC2InstanceRecommendations", true},
		{"compute-optimizer:Get*Recommendations", "compute-optimizer:GetEnrollmentStatus", false},
		{"servicecatalog:ListApplications", "servicecatalog:ListAssociatedResources", false},
		{"iam:GetRole", "iam:GetRolePolicy", false},
		{"s3:ListAllMyBuckets", "s3:listallmybuckets", true},
	} {
		if got := iamMatch(testCase.pattern, testCase.action); got != testCase.want {
			t.Errorf("iamMatch(%q, %q) = %v, want %v", testCase.pattern, testCase.action, got, testCase.want)
		}
	}
}
