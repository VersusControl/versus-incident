package changes

import "strings"

// ResourceKey is the form two canonical ARNs are compared in. It returns ""
// for anything that is not a well-formed ARN, so a short name never
// matches. It folds the ARNs AWS gives one resource:
//   - an Auto Scaling group ARN with its generated ID and the name form;
//   - a CloudFormation stack ARN with its stack ID and the name form;
//   - an IAM role ARN with and without its path;
//   - a Lambda function ARN with and without a version or alias;
//   - RDS identifiers in any letter case.
//
// It is the only folding: the change list's resource filter, before values
// and the suspect engine (suspect.Key) all use it, so an inventory ARN and a
// change's ARN of one resource match everywhere.
func ResourceKey(arn string) string {
	canonical, ok := CanonicalARN(arn)
	if !ok {
		return ""
	}
	parts := strings.SplitN(canonical, ":", 6)
	service, resource := parts[2], parts[5]
	switch service {
	case "autoscaling":
		if strings.HasPrefix(resource, "autoScalingGroup:") {
			if index := strings.Index(resource, ":autoScalingGroupName/"); index >= 0 {
				resource = "autoScalingGroup:*" + resource[index:]
			}
		}
	case "cloudformation":
		if rest, found := strings.CutPrefix(resource, "stack/"); found {
			name, _, _ := strings.Cut(rest, "/")
			resource = "stack/" + name + "/*"
		}
	case "iam":
		if strings.HasPrefix(resource, "role/") {
			resource = "role/" + resource[strings.LastIndex(resource, "/")+1:]
		}
	case "rds":
		resource = strings.ToLower(resource)
	}
	parts[5] = resource
	return strings.Join(parts, ":")
}

// ResourceKeys folds every resource of a change, without duplicates.
func ResourceKeys(arns []string) []string {
	var out []string
	for _, arn := range arns {
		if key := ResourceKey(arn); key != "" && !contains(out, key) {
			out = append(out, key)
		}
	}
	return out
}
