package fakeaws

// Canary values the secret-canary fixture plants in resource metadata. A
// redaction test asserts none of them ever leaves the product surface.
const (
	CanaryTagValue         = "vrs-canary-tag-do-not-leak"
	CanaryEnvValue         = "vrs-canary-env-do-not-leak"
	CanaryAccessKey        = "AKIAIOSFODNN7EXAMPLE"
	CanaryRequestParameter = "vrs-canary-param-do-not-leak"
	CanaryAccountID        = "999988887777"
	CanaryRoleARN          = "arn:aws:iam::999988887777:role/versus-canary"
)

// CanarySecrets returns every planted value, so a test can fail when one
// appears in an output that should have been redacted.
func CanarySecrets() []string {
	return []string{CanaryTagValue, CanaryEnvValue, CanaryAccessKey, CanaryRequestParameter}
}

func (s *Server) canaryEnabled(request awsRequest) bool {
	return request.scenario == "secret-canary"
}

// canaryTagSet is the extra EC2 tag set the canary fixture adds beside the
// harmless name tag.
func canaryEC2Tags() string {
	return `<item><key>notes</key><value>` + CanaryTagValue + `</value></item>` +
		`<item><key>access_key</key><value>` + CanaryAccessKey + `</value></item>`
}

func canaryLambdaEnvironment() string {
	return `,"Environment":{"Variables":{"DB_PASSWORD":` + quote(CanaryEnvValue) +
		`,"ACCESS_KEY":` + quote(CanaryAccessKey) + `}}`
}

func canaryCloudTrailEvent() string {
	return `"CloudTrailEvent":"{\"requestParameters\":{\"password\":\"` +
		CanaryRequestParameter + `\"}}"`
}
