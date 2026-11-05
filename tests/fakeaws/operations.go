package fakeaws

import (
	"net/http"
)

// writeOperation answers one non-STS request with the smallest valid page for
// its operation.
func (s *Server) writeOperation(writer http.ResponseWriter, request awsRequest) {
	if request.service == "sts" {
		s.writeSTS(writer, request)
		return
	}
	if request.protocol == protocolCBOR {
		s.writeCBOROperation(writer, request)
		return
	}
	if request.service == "ec2" && request.action == "DescribeRegions" {
		s.writeDescribeRegions(writer, request)
		return
	}
	empty := s.emptyForAccount(request)

	switch request.service {
	case "apigateway":
		writeRESTJSONBody(writer, restJSONPage("item", `{"id":"abc123","name":"versus-api","createdDate":1720000000}`, empty))
	case "autoscaling":
		writeXMLBody(writer, queryResult("DescribeAutoScalingGroups", xmlPage("AutoScalingGroups", "member",
			`<AutoScalingGroupName>versus-web</AutoScalingGroupName>`+
				`<AutoScalingGroupARN>arn:aws:autoscaling:us-east-1:111122223333:autoScalingGroup:00000000-0000-0000-0000-000000000000:autoScalingGroupName/versus-web</AutoScalingGroupARN>`+
				`<MinSize>1</MinSize><MaxSize>4</MaxSize><DesiredCapacity>2</DesiredCapacity>`+
				`<HealthCheckType>EC2</HealthCheckType>`, empty)))
	case "health":
		s.writeHealth(writer, request, empty)
	case "cloudformation":
		writeXMLBody(writer, queryResult("DescribeStacks", xmlPage("Stacks", "member",
			`<StackName>versus-stack</StackName>`+
				`<StackId>arn:aws:cloudformation:us-east-1:111122223333:stack/versus-stack/00000000-0000-0000-0000-000000000000</StackId>`+
				`<StackStatus>CREATE_COMPLETE</StackStatus>`+
				xmlText("CreationTime", nowISO()), empty)))
	case "cloudfront":
		writeRESTXMLBody(writer, `<DistributionList xmlns="http://cloudfront.amazonaws.com/doc/2020-05-31/">`+
			`<Marker></Marker><MaxItems>1</MaxItems><IsTruncated>false</IsTruncated>`+
			`<Quantity>`+quantity(empty)+`</Quantity>`+itemsXML("Items", "DistributionSummary", empty,
			`<Id>EDFDVBD6EXAMPLE</Id>`+
				`<ARN>arn:aws:cloudfront::111122223333:distribution/EDFDVBD6EXAMPLE</ARN>`+
				`<Status>Deployed</Status>`+
				`<DomainName>d111111abcdef8.cloudfront.net</DomainName>`+
				`<Enabled>true</Enabled>`)+`</DistributionList>`)
	case "cloudtrail":
		event := `{"EventId":"00000000-0000-0000-0000-000000000001","EventName":"RunInstances",` +
			`"EventTime":1720000000,"Username":"versus"`
		if s.canaryEnabled(request) {
			event += "," + canaryCloudTrailEvent()
		}
		writeJSONBody(writer, jsonPage("Events", event+"}", empty))
	case "cloudwatch":
		writeXMLBody(writer, queryResult("DescribeAlarms", xmlPage("MetricAlarms", "member",
			`<AlarmName>versus-health</AlarmName><StateValue>OK</StateValue>`+
				`<MetricName>CPUUtilization</MetricName><Namespace>AWS/EC2</Namespace>`, empty)))
	case "config":
		writeJSONBody(writer, jsonPage("ConfigurationRecorders",
			`{"name":"default","roleARN":"arn:aws:iam::111122223333:role/versus-config"}`, empty))
	case "dynamodb":
		writeJSONBody(writer, jsonPage("TableNames", `"versus-locks"`, empty))
	case "ec2":
		writeXMLBody(writer, queryResult("DescribeInstances", ec2InstancesPage(empty, s.canaryEnabled(request))))
	case "ecs":
		writeJSONBody(writer, jsonPage("clusterArns", `"arn:aws:ecs:`+xmlEscape(request.region)+`:111122223333:cluster/versus"`, empty))
	case "eks":
		writeRESTJSONBody(writer, jsonPage("clusters", `"versus-eks"`, empty))
	case "elasticache":
		writeXMLBody(writer, queryResult("DescribeCacheClusters", xmlPage("CacheClusters", "CacheCluster",
			`<CacheClusterId>versus-cache</CacheClusterId>`+
				`<CacheNodeType>cache.t3.micro</CacheNodeType><Engine>redis</Engine>`+
				`<CacheClusterStatus>available</CacheClusterStatus><NumCacheNodes>1</NumCacheNodes>`, empty)))
	case "elasticloadbalancing":
		writeXMLBody(writer, queryResult("DescribeLoadBalancers", xmlPage("LoadBalancers", "member",
			`<LoadBalancerArn>arn:aws:elasticloadbalancing:`+xmlEscape(request.region)+`:111122223333:loadbalancer/app/versus/50dc6c495c0c9188</LoadBalancerArn>`+
				`<LoadBalancerName>versus</LoadBalancerName>`+
				`<DNSName>versus-1234567890.`+xmlEscape(request.region)+`.elb.amazonaws.com</DNSName>`+
				`<Scheme>internet-facing</Scheme><Type>application</Type>`+
				`<State><Code>active</Code></State>`, empty)))
	case "events":
		writeJSONBody(writer, jsonPage("Rules",
			`{"Name":"versus-checkpoint","Arn":"arn:aws:events:`+xmlEscape(request.region)+`:111122223333:rule/versus-checkpoint","State":"ENABLED","EventPattern":"{}"}`, empty))
	case "iam":
		writeXMLBody(writer, queryResult("ListRoles", xmlPage("Roles", "member",
			`<RoleName>versus-reader</RoleName><Path>/</Path>`+
				`<Arn>arn:aws:iam::111122223333:role/versus-reader</Arn>`+
				xmlText("CreateDate", nowISO()), empty)))
	case "kinesis":
		if empty {
			writeJSONBody(writer, `{"StreamNames":[],"HasMoreStreams":false}`)
			return
		}
		writeJSONBody(writer, `{"StreamNames":["versus-events"],"HasMoreStreams":false}`)
	case "lambda":
		function := `{"FunctionName":"versus-worker","FunctionArn":"arn:aws:lambda:` +
			xmlEscape(request.region) + `:111122223333:function:versus-worker",` +
			`"Runtime":"provided.al2023","State":"Active"`
		if s.canaryEnabled(request) {
			function += canaryLambdaEnvironment()
		}
		writeRESTJSONBody(writer, jsonPage("Functions", function+"}", empty))
	case "rds":
		writeXMLBody(writer, queryResult("DescribeDBInstances", xmlPage("DBInstances", "DBInstance",
			`<DBInstanceIdentifier>versus-db</DBInstanceIdentifier>`+
				`<DBInstanceClass>db.t3.micro</DBInstanceClass><Engine>postgres</Engine>`+
				`<DBInstanceStatus>available</DBInstanceStatus>`+
				`<AvailabilityZone>`+xmlEscape(request.region)+`a</AvailabilityZone>`+
				`<DBInstanceArn>arn:aws:rds:`+xmlEscape(request.region)+`:111122223333:db:versus-db</DBInstanceArn>`, empty)))
	case "route53":
		writeRESTXMLBody(writer, `<ListHealthChecksResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/">`+
			`<HealthChecks>`+itemsXML("", "HealthCheck", empty,
			`<Id>abcd1234-0000-0000-0000-000000000000</Id><CallerReference>versus</CallerReference>`+
				`<HealthCheckConfig><Type>HTTPS</Type>`+
				`<FullyQualifiedDomainName>example.com</FullyQualifiedDomainName></HealthCheckConfig>`)+`</HealthChecks>`+
			`<Marker></Marker><IsTruncated>false</IsTruncated><MaxItems>1</MaxItems>`+
			`</ListHealthChecksResponse>`)
	case "s3":
		writeRESTXMLBody(writer, `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
			`<Owner><ID>versus</ID></Owner>`+itemsXML("Buckets", "Bucket", empty,
			`<Name>versus-logs</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate>`)+`</ListAllMyBucketsResult>`)
	case "sns":
		writeXMLBody(writer, queryResult("ListTopics", xmlPage("Topics", "member",
			`<TopicArn>arn:aws:sns:`+xmlEscape(request.region)+`:111122223333:versus-alarms</TopicArn>`, empty)))
	case "sqs":
		if empty {
			writeJSONBody(writer, `{"QueueUrls":[]}`)
			return
		}
		writeJSONBody(writer, `{"QueueUrls":["https://sqs.`+xmlEscape(request.region)+`.amazonaws.com/111122223333/versus-jobs"]}`)
	case "states":
		writeJSONBody(writer, jsonPage("stateMachines",
			`{"stateMachineArn":"arn:aws:states:`+xmlEscape(request.region)+`:111122223333:stateMachine:versus-checkpoint","name":"versus-checkpoint","type":"STANDARD","creationDate":1720000000}`, empty))
	default:
		s.writeError(writer, request.protocol, http.StatusBadRequest, "InvalidAction",
			"The action "+xmlEscape(request.operation())+" is not valid for this endpoint")
	}
}

// xmlPage renders one item, or none, inside a wrapper element.
func xmlPage(wrapper, item, inner string, empty bool) string {
	if empty {
		return "<" + wrapper + "></" + wrapper + ">"
	}
	return "<" + wrapper + "><" + item + ">" + inner + "</" + item + "></" + wrapper + ">"
}

// xmlPageList renders text values inside a wrapper element.
func xmlPageList(wrapper, item string, values []string, empty bool) string {
	inner := ""
	if !empty {
		for _, value := range values {
			inner += "<" + item + ">" + xmlEscape(value) + "</" + item + ">"
		}
	}
	if wrapper == "" {
		return inner
	}
	return "<" + wrapper + ">" + inner + "</" + wrapper + ">"
}

func itemsXML(wrapper, item string, empty bool, inner string) string {
	element := "<" + item + ">" + inner + "</" + item + ">"
	if wrapper == "" {
		if empty {
			return ""
		}
		return element
	}
	if empty {
		return "<" + wrapper + "></" + wrapper + ">"
	}
	return "<" + wrapper + ">" + element + "</" + wrapper + ">"
}

func quantity(empty bool) string {
	if empty {
		return "0"
	}
	return "1"
}

func restJSONPage(key, item string, empty bool) string {
	if empty {
		return `{"` + key + `":[]}`
	}
	return `{"` + key + `":[` + item + `]}`
}

func jsonPage(key, item string, empty bool) string {
	return restJSONPage(key, item, empty)
}

func ec2InstancesPage(empty, canary bool) string {
	if empty {
		return "<reservationSet></reservationSet>"
	}
	tags := "<tagSet><item><key>Name</key><value>versus-web</value></item></tagSet>"
	if canary {
		tags = "<tagSet><item><key>Name</key><value>versus-web</value></item>" + canaryEC2Tags() + "</tagSet>"
	}
	return "<reservationSet><item>" +
		`<reservationId>r-0123456789abcdef0</reservationId>` +
		`<ownerId>111122223333</ownerId>` +
		"<instancesSet><item>" +
		`<instanceId>i-0123456789abcdef0</instanceId>` +
		`<instanceType>t3.micro</instanceType>` +
		`<instanceState><code>16</code><name>running</name></instanceState>` +
		`<privateIpAddress>10.0.1.10</privateIpAddress>` +
		tags +
		"</item></instancesSet></item></reservationSet>"
}

// writeCBOROperation answers the rpc-v2-cbor services the probe grid reads.
func (s *Server) writeCBOROperation(writer http.ResponseWriter, request awsRequest) {
	switch request.service {
	case "health":
		if request.account.HealthNotOnPlan {
			writeCBORError(writer, http.StatusBadRequest, "SubscriptionRequiredException",
				"AWS Health requires a Business or Enterprise support plan")
			return
		}
		if s.emptyForAccount(request) {
			writeCBORBody(writer, http.StatusOK, map[string]any{"events": []any{}})
			return
		}
		writeCBORBody(writer, http.StatusOK, map[string]any{"events": []any{map[string]any{}}})
	case "cloudwatch":
		if s.emptyForAccount(request) {
			writeCBORBody(writer, http.StatusOK, map[string]any{"MetricAlarms": []any{}, "CompositeAlarms": []any{}})
			return
		}
		writeCBORBody(writer, http.StatusOK, map[string]any{"MetricAlarms": []any{map[string]any{}}, "CompositeAlarms": []any{}})
	default:
		writeCBORError(writer, http.StatusBadRequest, "InvalidAction", "Unsupported CBOR operation")
	}
}

func (s *Server) writeDescribeRegions(writer http.ResponseWriter, request awsRequest) {
	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-southeast-1"}
	inner := ""
	for _, region := range regions {
		inner += "<item>" + xmlText("regionName", region) +
			xmlText("regionEndpoint", "ec2."+region+".amazonaws.com") +
			xmlText("optInStatus", "opt-in-not-required") + "</item>"
	}
	body := `<DescribeRegionsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` +
		`<requestId>` + requestID + `</requestId><regionInfo>` + inner + `</regionInfo>` +
		`</DescribeRegionsResponse>`
	writeXMLBody(writer, body)
}

func (s *Server) writeHealth(writer http.ResponseWriter, request awsRequest, empty bool) {
	if request.account.HealthNotOnPlan {
		s.writeError(writer, request.protocol, http.StatusBadRequest, "SubscriptionRequiredException",
			"AWS Health requires a Business or Enterprise support plan")
		return
	}
	event := `{"arn":"arn:aws:health:us-east-1::event/EC2/AWS_EC2_OPERATIONAL_ISSUE/AWS_EC2_OPERATIONAL_ISSUE_0000000000",` +
		`"service":"EC2","eventTypeCode":"AWS_EC2_OPERATIONAL_ISSUE","eventTypeCategory":"issue",` +
		`"region":"us-east-1","startTime":1720000000,"statusCode":"open","eventScopeCode":"PUBLIC"}`
	if request.protocol == protocolQuery {
		if empty {
			writeXMLBody(writer, queryResult("DescribeEvents", "<events></events>"))
			return
		}
		writeXMLBody(writer, queryResult("DescribeEvents", "<events><event>"+
			"<arn>arn:aws:health:us-east-1::event/EC2/AWS_EC2_OPERATIONAL_ISSUE/AWS_EC2_OPERATIONAL_ISSUE_0000000000</arn>"+
			"<service>EC2</service><eventTypeCode>AWS_EC2_OPERATIONAL_ISSUE</eventTypeCode>"+
			"<eventTypeCategory>issue</eventTypeCategory><region>us-east-1</region>"+
			"<startTime>1720000000</startTime><statusCode>open</statusCode>"+
			"<eventScopeCode>PUBLIC</eventScopeCode></event></events>"))
		return
	}
	if empty {
		writeJSONBody(writer, `{"events":[]}`)
		return
	}
	writeJSONBody(writer, `{"events":[`+event+`]}`)
}
