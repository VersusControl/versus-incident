## Slack Template for AWS SNS

### 1. AWS Glue Job Failure  
**SNS Message:**  
```json
{
  "source": "aws.glue",
  "detail": {
    "jobName": "etl-pipeline",
    "state": "FAILED",
    "errorMessage": "OutOfMemoryError: Java heap space"
  }
}
```

**Slack Template:**  
```gotemplate
{{ if eq .source "aws.glue" }}
🔥 *Glue Job Failed*: {{.detail.jobName}}
❌ Error: 
```{{.detail.errorMessage}}```
{{ end }}
```

---

### 2. EC2 Instance State Change  
**SNS Message:**  
```json
{
  "source": "aws.ec2",
  "detail": {
    "instance-id": "i-1234567890abcdef0",
    "state": "stopped"
  }
}
```

**Slack Template:**  
```gotemplate
{{ if eq .source "aws.ec2" }}
🖥 *EC2 Instance {{.detail.state | title}}*
ID: `{{.detail.instance-id}}`
{{ end }}
```

---

### 3. CloudWatch Alarm Trigger  
**SNS Message:**  
```json
{
  "source": "aws.cloudwatch",
  "detail": {
    "alarmName": "High-CPU-Utilization",
    "state": "ALARM",
    "metricName": "CPUUtilization",
    "threshold": 80,
    "actualValue": 92.5
  }
}
```

**Slack Template:**  
```gotemplate
{{ if eq .source "aws.cloudwatch" }}
🚨 *CloudWatch Alarm Triggered*
• Name: {{.detail.alarmName}}
• Metric: {{.detail.metricName}}
• Value: {{.detail.actualValue}}% (Threshold: {{.detail.threshold}}%)
{{ end }}
```

---

### 4. Lambda Function Error  
**SNS Message:**  
```json
{
  "source": "aws.lambda",
  "detail": {
    "functionName": "data-processor",
    "errorType": "Runtime.ExitError",
    "errorMessage": "Process exited before completing request"
  }
}
```

**Slack Template:**  
```gotemplate
{{ if eq .source "aws.lambda" }}
λ *Lambda Failure*: {{.detail.functionName}}
⚠️ Error: {{.detail.errorType}}
💬 Message: {{.detail.errorMessage}}
{{ end }}
```

---

### 5. **AWS CodePipeline Failure**  
**Scenario**: A pipeline deployment fails during the "Deploy" stage.  

**SNS Message**:  
```json  
{
  "source": "aws.codepipeline",
  "detail-type": "CodePipeline Pipeline Execution State Change",
  "detail": {
    "pipeline": "prod-deployment-pipeline",
    "state": "FAILED",
    "stage": "Deploy",
    "action": "DeployToECS",
    "failure-type": "JobFailed",
    "error": "ECS task definition invalid"
  }
}
```  

**Slack Template**:  
```gotemplate  
{{ if eq .source "aws.codepipeline" }}
🚛 *Pipeline Failed*: {{.detail.pipeline | upper}}  
🛑 Stage: {{.detail.stage}} (Action: {{.detail.action}})  
❌ Error:  
```{{.detail.error}}```  
{{ end }}
```  

---

### 6. **EC2 Spot Instance Interruption (via EventBridge)**  
**Scenario**: AWS reclaims a Spot Instance due to capacity needs.  

**SNS Message**:  
```json  
{
  "source": "aws.ec2",
  "detail-type": "EC2 Spot Instance Interruption Warning",
  "detail": {
    "instance-id": "i-0abcdef1234567890",
    "instance-action": "terminate",
    "instance-interruption-behavior": "terminate",
    "availability-zone": "us-east-1a",
    "instance-type": "r5.large"
  }
}
```  

**Slack Template**:  
```gotemplate  
{{ if eq .detail-type "EC2 Spot Instance Interruption Warning" }}
⚡ *Spot Instance Interruption*  
Instance ID: `{{.detail.instance-id}}`  
Action: {{.detail.instance-action | title}}  
AZ: {{.detail.availability-zone}}  
⚠️ **Warning**: Migrate workloads immediately!  
{{ end }}
```  

---

### 7. **ECS Task Failure**  
**Scenario**: A critical ECS task crashes repeatedly.  

**SNS Message**:  
```json  
{
  "source": "aws.ecs",
  "detail-type": "ECS Task State Change",
  "detail": {
    "clusterArn": "arn:aws:ecs:us-east-1:123456789012:cluster/prod-cluster",
    "taskArn": "arn:aws:ecs:us-east-1:123456789012:task/prod-cluster/abc123",
    "lastStatus": "STOPPED",
    "stoppedReason": "Essential container exited"
  }
}
```  

**Slack Template**:  
```gotemplate  
{{ if eq .source "aws.ecs" }}
🎯 *ECS Task Stopped*  
Cluster: {{.detail.clusterArn | splitList "/" | last}}  
Reason:  
```{{.detail.stoppedReason}}```  
{{ end }}
```  

---

### 8. **DynamoDB Auto-Scaling Limit Reached**  
**Scenario**: DynamoDB hits provisioned throughput limits.  

**SNS Message**:  
```json  
{
  "source": "aws.dynamodb",
  "detail-type": "AWS API Call via CloudTrail",
  "detail": {
    "eventSource": "dynamodb.amazonaws.com",
    "eventName": "UpdateTable",
    "errorCode": "LimitExceededException",
    "errorMessage": "Table my-table exceeded maximum allowed provisioned throughput"
  }
}
```  

**Slack Template**:  
```gotemplate  
{{ if and (eq .source "aws.dynamodb") (eq .detail.errorCode "LimitExceededException") }}
📊 *DynamoDB Throughput Limit Exceeded*  
Table: `{{.detail.requestParameters.tableName}}`  
Error:  
```{{.detail.errorMessage}}```  
{{ end }}
```  

---

### 9. **AWS Health Event (Service Disruption)**  
**Scenario**: AWS reports a regional service disruption.  

**SNS Message**:  
```json  
{
  "source": "aws.health",
  "detail-type": "AWS Health Event",
  "detail": {
    "eventTypeCategory": "issue",
    "service": "EC2",
    "eventDescription": [{
      "language": "en",
      "latestDescription": "Degraded networking in us-east-1"
    }]
  }
}
```  

**Slack Template**:  
```gotemplate  
{{ if eq .source "aws.health" }}
🏥 *AWS Health Alert*  
Service: {{.detail.service}}  
Impact: {{.detail.eventTypeCategory | title}}  
Description:  
{{index .detail.eventDescription 0).latestDescription}}  
{{ end }}
```  

---

### 10. **Amazon GuardDuty Finding**  
**Scenario**: Unauthorized API call detected.  

**SNS Message**:  
```json  
{
  "source": "aws.guardduty",
  "detail-type": "GuardDuty Finding",
  "detail": {
    "severity": 8.5,
    "type": "UnauthorizedAccess:EC2/SSHBruteForce",
    "resource": {
      "instanceDetails": {
        "instanceId": "i-0abcdef1234567890"
      }
    }
  }
}
```  

**Slack Template**:  
```gotemplate  
{{ if eq .source "aws.guardduty" }}
🛡️ *Security Alert*: {{.detail.type | replace "UnauthorizedAccess:" ""}}  
Severity: {{.detail.severity}}/10  
Instance: `{{.detail.resource.instanceDetails.instanceId}}`  
{{ end }}
```  

---

## Test Templates Locally

Use the AWS CLI to send test SNS messages:  
```bash  
aws sns publish \
  --topic-arn arn:aws:sns:us-east-1:123456789012:MyTopic \
  --message file://test-event.json
```  
